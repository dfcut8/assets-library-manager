package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/imageinspect"
)

const maxProgressFailures = 50

// CoordinatorConfig contains the explicit processing and archive limits for one startup scan.
type CoordinatorConfig struct {
	Workers          int
	MaxSourceBytes   int64
	InspectionLimits imageinspect.Limits
	ArchiveLimits    ArchiveLimits
}

// ProgressFailure is one bounded user-safe source or item failure.
type ProgressFailure struct {
	Source  string
	Item    string
	Code    ErrorCode
	Message string
}

// Progress is an immutable snapshot of the single startup scan.
type Progress struct {
	Active           bool
	StartedAt        time.Time
	CompletedAt      time.Time
	SourcesTotal     int
	SourcesCompleted int
	SourcesDeleted   int
	SourcesRetained  int
	ItemsTotal       int
	ItemsReady       int
	ItemsDuplicate   int
	ItemsBlocked     int
	ItemsFailed      int
	Failures         []ProgressFailure
}

// WorkflowRepository is the coordinator-owned persistence contract.
type WorkflowRepository interface {
	CreateImportSource(context.Context, SourceRecord) (SourceRecord, error)
	CreateImportItem(context.Context, ItemRecord) (ItemRecord, error)
	FindImportItem(context.Context, ID) (ItemRecord, error)
	TransitionImportItem(context.Context, ItemTransition) error
	TransitionImportSource(context.Context, SourceTransition) error
	FindReadyByDigest(context.Context, Digest) (AssetRef, error)
	CommitStagedAsset(context.Context, StagedAsset) error
	MarkAssetReady(context.Context, ID, ID, time.Time) error
	SummarizeSource(context.Context, ID) (SourceSummary, error)
	ListRecoverableItems(context.Context) ([]ItemRecord, error)
	ListRecoveryAssets(context.Context) ([]RecoveryAsset, error)
	ListReferencedStagedPaths(context.Context) ([]StagedPath, error)
	ListPendingDeletions(context.Context) ([]PendingDeletion, error)
	FailStagedAsset(context.Context, ID, ID, ErrorCode, string, time.Time) error
	MarkAssetIntegrityFailed(context.Context, ID, time.Time) error
}

// ProcessingStore is the root-scoped filesystem contract consumed by the coordinator.
type ProcessingStore interface {
	IncomingSnapshotter
	FingerprintIncoming(context.Context, SourcePath, int64) (Digest, int64, error)
	OpenIncoming(SourcePath) (*os.File, error)
	Stage(context.Context, ID, io.Reader, int64) (StagedFile, error)
	OpenStaged(StagedPath) (*os.File, error)
	VerifyStaged(context.Context, StagedPath, Digest, int64) (bool, error)
	RemoveStaged(StagedPath) error
	Promote(context.Context, StagedFile, ManagedPath) error
	VerifyManaged(context.Context, ManagedPath, Digest, int64) (bool, error)
	DeleteIncoming(context.Context, SourcePath, Digest, int64) error
	CreateAnalysisScratch(ID, string, []byte) (ScratchImage, error)
	RemoveAnalysisScratch(ID) error
	CleanOrphanStaging(context.Context, []StagedPath, time.Time) ([]StagedPath, error)
}

// ImageInspector validates images and creates derived renditions.
type ImageInspector interface {
	Inspect(context.Context, io.ReadSeeker, imageinspect.Limits) (imageinspect.Inspection, error)
}

// SemanticAnalyzer obtains required catalog metadata from the owned App Server process.
type SemanticAnalyzer interface {
	Analyze(context.Context, ImageInput) (AnalysisResult, AnalysisProvenance, error)
}

type reservation struct {
	done      chan struct{}
	asset     AssetRef
	err       error
	completed bool
}

// Coordinator owns one startup snapshot, its bounded worker queue, reservations, and progress.
type Coordinator struct {
	config     CoordinatorConfig
	repository WorkflowRepository
	store      ProcessingStore
	inspector  ImageInspector
	analyzer   SemanticAnalyzer
	logger     *slog.Logger
	now        func() time.Time

	progressMu sync.RWMutex
	progress   Progress
	runMu      sync.Mutex
	runStarted bool

	reservationMu sync.Mutex
	reservations  map[Digest]*reservation
	stopped       bool
}

// NewCoordinator validates dependencies and constructs a single-use startup coordinator.
func NewCoordinator(
	config CoordinatorConfig,
	repository WorkflowRepository,
	store ProcessingStore,
	inspector ImageInspector,
	analyzer SemanticAnalyzer,
	logger *slog.Logger,
) (*Coordinator, error) {
	if config.Workers < 1 || config.Workers > 32 || config.MaxSourceBytes < 1 ||
		config.InspectionLimits.MaxSourceBytes < 1 ||
		validateArchiveLimits(config.ArchiveLimits) != nil {
		return nil, errors.New("creating import coordinator: configuration is invalid")
	}
	if repository == nil || store == nil || inspector == nil {
		return nil, errors.New("creating import coordinator: dependencies are incomplete")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Coordinator{
		config: config, repository: repository, store: store,
		inspector: inspector, analyzer: analyzer, logger: logger,
		now: time.Now, reservations: make(map[Digest]*reservation),
	}, nil
}

// Snapshot returns a defensive immutable copy safe for concurrent handlers.
func (coordinator *Coordinator) Snapshot() Progress {
	coordinator.progressMu.RLock()
	defer coordinator.progressMu.RUnlock()
	snapshot := coordinator.progress
	snapshot.Failures = append([]ProgressFailure(nil), coordinator.progress.Failures...)

	return snapshot
}

// StopReservations prevents new digest work and releases every waiting worker.
func (coordinator *Coordinator) StopReservations() {
	coordinator.reservationMu.Lock()
	defer coordinator.reservationMu.Unlock()
	if coordinator.stopped {
		return
	}
	coordinator.stopped = true
	for digest, entry := range coordinator.reservations {
		entry.err = context.Canceled
		entry.completed = true
		close(entry.done)
		delete(coordinator.reservations, digest)
	}
}

func (coordinator *Coordinator) reserve(ctx context.Context, digest Digest) (bool, *reservation, error) {
	coordinator.reservationMu.Lock()
	if coordinator.stopped {
		coordinator.reservationMu.Unlock()

		return false, nil, context.Canceled
	}
	if existing := coordinator.reservations[digest]; existing != nil {
		coordinator.reservationMu.Unlock()
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		case <-existing.done:
			return false, existing, existing.err
		}
	}
	entry := &reservation{done: make(chan struct{})}
	coordinator.reservations[digest] = entry
	coordinator.reservationMu.Unlock()

	return true, entry, nil
}

func (coordinator *Coordinator) finishReservation(
	digest Digest,
	entry *reservation,
	asset AssetRef,
	err error,
) {
	coordinator.reservationMu.Lock()
	defer coordinator.reservationMu.Unlock()
	if entry.completed {
		return
	}
	entry.asset = asset
	entry.err = err
	entry.completed = true
	close(entry.done)
	if coordinator.reservations[digest] == entry {
		delete(coordinator.reservations, digest)
	}
}

func (coordinator *Coordinator) updateProgress(update func(*Progress)) {
	coordinator.progressMu.Lock()
	defer coordinator.progressMu.Unlock()
	update(&coordinator.progress)
}

func (coordinator *Coordinator) addFailure(failure ProgressFailure) {
	coordinator.updateProgress(func(progress *Progress) {
		if len(progress.Failures) < maxProgressFailures {
			progress.Failures = append(progress.Failures, failure)
		}
	})
	coordinator.logger.Error(
		"import processing failed",
		"source", failure.Source,
		"item", failure.Item,
		"code", failure.Code,
		"message", failure.Message,
	)
}

func newWorkflowID() (ID, error) {
	id, err := NewID()
	if err != nil {
		return ID{}, fmt.Errorf("creating workflow identifier: %w", err)
	}

	return id, nil
}
