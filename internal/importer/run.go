package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sync"
)

type workItem struct {
	source       SourceRecord
	item         ItemRecord
	sourcePath   SourcePath
	extension    string
	originalName string
	staged       StagedFile
}

type itemResult struct {
	sourceID ID
	path     SourcePath
	itemName string
	state    ItemState
	code     ErrorCode
	message  string
}

type sourceRun struct {
	record      SourceRecord
	kind        CandidateKind
	pending     int
	scanDone    bool
	supported   int
	unhandled   int
	scanFailure string
	finalized   bool
	skip        bool
}

// Run executes exactly one startup snapshot with a bounded queue and fixed worker count.
func (coordinator *Coordinator) Run(ctx context.Context) error {
	coordinator.runMu.Lock()
	if coordinator.runStarted {
		coordinator.runMu.Unlock()

		return errors.New("running import coordinator: startup scan already ran")
	}
	coordinator.runStarted = true
	coordinator.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	coordinator.updateProgress(func(progress *Progress) {
		*progress = Progress{Active: true, StartedAt: coordinator.now().UTC()}
	})
	discovery, err := Discover(ctx, coordinator.store)
	if err != nil {
		coordinator.completeProgress()
		return err
	}
	coordinator.updateProgress(func(progress *Progress) {
		progress.SourcesTotal = len(discovery.Sources) + len(discovery.Failures)
	})
	for _, failure := range discovery.Failures {
		coordinator.addFailure(ProgressFailure{
			Source: failure.Name, Code: ErrorCodeInvalidInput, Message: failure.Message,
		})
		coordinator.updateProgress(func(progress *Progress) {
			progress.SourcesCompleted++
			progress.SourcesRetained++
		})
	}

	queue := make(chan workItem, 2*coordinator.config.Workers)
	results := make(chan itemResult, 2*coordinator.config.Workers)
	var workers sync.WaitGroup
	for range coordinator.config.Workers {
		workers.Go(func() { coordinator.worker(ctx, queue, results) })
	}
	sources := make(map[ID]*sourceRun, len(discovery.Sources))
	pending := 0
	queueClosed := false
	closeQueue := func() {
		if !queueClosed {
			close(queue)
			queueClosed = true
		}
	}
	defer closeQueue()

	handleResult := func(result itemResult) {
		source := sources[result.sourceID]
		if source == nil || source.pending < 1 {
			return
		}
		source.pending--
		pending--
		coordinator.recordItemResult(result)
		if source.scanDone && source.pending == 0 {
			coordinator.finalizeSource(ctx, source)
		}
	}
	enqueue := func(work workItem) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case result := <-results:
				handleResult(result)
			case queue <- work:
				source := sources[work.source.ID]
				source.pending++
				pending++

				return nil
			}
		}
	}

	for _, candidate := range discovery.Sources {
		if err := ctx.Err(); err != nil {
			break
		}
		source, prepareErr := coordinator.prepareSource(ctx, candidate)
		if prepareErr != nil {
			coordinator.recordSourcePreparationFailure(candidate, prepareErr)
			continue
		}
		sources[source.record.ID] = source
		if source.skip {
			coordinator.completeSource(false, true)
			continue
		}
		scanErr := coordinator.scanSource(ctx, candidate, source, enqueue)
		if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
			source.scanFailure = userSafeScanFailure(scanErr)
		}
		source.scanDone = true
		if source.pending == 0 {
			coordinator.finalizeSource(ctx, source)
		}
	}
	closeQueue()
	if ctx.Err() == nil {
		for pending > 0 {
			select {
			case <-ctx.Done():
				coordinator.StopReservations()
				pending = 0
			case result := <-results:
				handleResult(result)
			}
		}
	} else {
		coordinator.StopReservations()
	}
	workers.Wait()
	coordinator.completeProgress()
	if err := ctx.Err(); err != nil {
		return err
	}

	return nil
}

func (coordinator *Coordinator) prepareSource(
	ctx context.Context,
	candidate SourceCandidate,
) (*sourceRun, error) {
	digest, _, fingerprintErr := coordinator.store.FingerprintIncoming(
		ctx, candidate.Path, coordinator.config.MaxSourceBytes,
	)
	if fingerprintErr != nil {
		return nil, fmt.Errorf("fingerprinting source: %w", fingerprintErr)
	}
	sourceID, err := newWorkflowID()
	if err != nil {
		return nil, err
	}
	now := coordinator.now().UTC()
	sourceType := SourceTypeLoose
	if candidate.Kind == CandidateKindZIP {
		sourceType = SourceTypeZIP
	}
	record, createErr := coordinator.repository.CreateImportSource(ctx, SourceRecord{
		ID: sourceID, Path: candidate.Path, Type: sourceType, DiscoveryFingerprint: digest,
		State: SourceStateDiscovered, DeletionState: DeletionStateNotEligible,
		DiscoveredAt: now, UpdatedAt: now,
	})
	if createErr != nil {
		return nil, createErr
	}
	if record.DeletionState == DeletionStateFailed {
		return &sourceRun{record: record, kind: candidate.Kind, skip: true}, nil
	}
	if record.State != SourceStateProcessing {
		if err := coordinator.transitionSource(
			ctx, &record, SourceStateProcessing, DeletionStateNotEligible, "", "", "",
		); err != nil {
			return nil, err
		}
	}

	return &sourceRun{record: record, kind: candidate.Kind}, nil
}

func (coordinator *Coordinator) scanSource(
	ctx context.Context,
	candidate SourceCandidate,
	source *sourceRun,
	enqueue func(workItem) error,
) error {
	switch candidate.Kind {
	case CandidateKindLoose:
		source.supported = 1
		item, err := coordinator.createItem(ctx, source.record, "")
		if err != nil {
			return err
		}
		coordinator.updateProgressForExisting(item)
		if item.State.Terminal() {
			return nil
		}
		if coordinator.analyzer == nil {
			return coordinator.blockDiscoveredItem(ctx, item, source.record.Path)
		}
		var staged StagedFile
		if item.State == ItemStateStaged {
			staged, err = coordinator.stagedFromRecord(item)
			if err != nil {
				return coordinator.failScannedItem(
					ctx, item, source.record.Path, ErrorCodeStorage, "staged image is unavailable",
				)
			}
		}

		return enqueue(workItem{
			source: source.record, item: item, sourcePath: candidate.Path,
			extension: candidate.Extension, originalName: path.Base(candidate.Path.String()),
			staged: staged,
		})
	case CandidateKindZIP:
		return coordinator.scanZIPSource(ctx, candidate, source, enqueue)
	case CandidateKindUnsupported:
		return errors.New("source extension is unsupported")
	default:
		return errors.New("source classification is invalid")
	}
}

func (coordinator *Coordinator) scanZIPSource(
	ctx context.Context,
	candidate SourceCandidate,
	source *sourceRun,
	enqueue func(workItem) error,
) (returnErr error) {
	archive, err := coordinator.store.OpenIncoming(candidate.Path)
	if err != nil {
		return err
	}
	defer func() {
		if err := archive.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing incoming zip: %w", err))
		}
	}()
	info, err := archive.Stat()
	if err != nil {
		return fmt.Errorf("stating incoming zip: %w", err)
	}
	report, scanErr := ScanZIP(
		ctx, archive, info.Size(), coordinator.config.ArchiveLimits,
		func(ctx context.Context, entry ZIPEntry, reader io.Reader) error {
			source.supported++
			item, err := coordinator.createItem(ctx, source.record, entry.Name)
			if err != nil {
				return err
			}
			coordinator.updateProgressForExisting(item)
			if item.State.Terminal() {
				return nil
			}
			if coordinator.analyzer == nil {
				return coordinator.blockDiscoveredItem(ctx, item, source.record.Path)
			}
			if item.State == ItemStateStaged {
				staged, err := coordinator.stagedFromRecord(item)
				if err != nil {
					return coordinator.failScannedItem(
						ctx, item, source.record.Path, ErrorCodeStorage, "staged image is unavailable",
					)
				}

				return enqueue(workItem{
					source: source.record, item: item, sourcePath: candidate.Path,
					extension: entry.Extension, originalName: path.Base(entry.Name), staged: staged,
				})
			}
			staged, err := coordinator.store.Stage(ctx, item.ID, reader, coordinator.config.MaxSourceBytes)
			if err != nil {
				return coordinator.failScannedItem(
					ctx, item, source.record.Path, ErrorCodeStorage, "image could not be staged",
				)
			}
			if err := coordinator.repository.TransitionImportItem(ctx, ItemTransition{
				ID: item.ID, From: ItemStateDiscovered, To: ItemStateStaged,
				StagedPath: staged.Path, Digest: staged.Digest, UpdatedAt: coordinator.now().UTC(),
			}); err != nil {
				_ = coordinator.store.RemoveStaged(staged.Path)

				return err
			}
			item.State = ItemStateStaged
			item.StagedPath = staged.Path
			item.Digest = staged.Digest

			return enqueue(workItem{
				source: source.record, item: item, sourcePath: candidate.Path,
				extension: entry.Extension, originalName: path.Base(entry.Name), staged: staged,
			})
		},
	)
	source.unhandled = report.UnhandledCount

	return scanErr
}

func (coordinator *Coordinator) createItem(
	ctx context.Context,
	source SourceRecord,
	entryName string,
) (ItemRecord, error) {
	id, err := newWorkflowID()
	if err != nil {
		return ItemRecord{}, err
	}
	now := coordinator.now().UTC()
	item, err := coordinator.repository.CreateImportItem(ctx, ItemRecord{
		ID: id, SourceID: source.ID, ZIPEntryName: entryName,
		State: ItemStateDiscovered, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return ItemRecord{}, err
	}
	coordinator.updateProgress(func(progress *Progress) { progress.ItemsTotal++ })

	return item, nil
}

func (coordinator *Coordinator) blockDiscoveredItem(
	ctx context.Context,
	item ItemRecord,
	sourcePath SourcePath,
) error {
	if item.State != ItemStateDiscovered {
		message := "Codex analysis is unavailable; configure ChatGPT authentication and restart"
		if err := coordinator.failItem(ctx, item, ErrorCodeCodexUnavailable, message); err != nil {
			return err
		}
		coordinator.recordItemResult(itemResult{
			sourceID: item.SourceID, path: sourcePath, itemName: item.ZIPEntryName,
			state: ItemStateFailed, code: ErrorCodeCodexUnavailable, message: message,
		})

		return nil
	}
	message := "Codex analysis is unavailable; configure ChatGPT authentication and restart"
	if err := coordinator.repository.TransitionImportItem(ctx, ItemTransition{
		ID: item.ID, From: ItemStateDiscovered, To: ItemStateBlocked,
		ErrorCode: ErrorCodeCodexUnavailable, ErrorMessage: message, UpdatedAt: coordinator.now().UTC(),
	}); err != nil {
		return err
	}
	coordinator.recordItemResult(itemResult{
		sourceID: item.SourceID, path: sourcePath, itemName: item.ZIPEntryName,
		state: ItemStateBlocked, code: ErrorCodeCodexUnavailable, message: message,
	})

	return nil
}

func (coordinator *Coordinator) failScannedItem(
	ctx context.Context,
	item ItemRecord,
	sourcePath SourcePath,
	code ErrorCode,
	message string,
) error {
	if err := coordinator.failItem(ctx, item, code, message); err != nil {
		return err
	}
	coordinator.recordItemResult(itemResult{
		sourceID: item.SourceID, path: sourcePath, itemName: item.ZIPEntryName,
		state: ItemStateFailed, code: code, message: message,
	})

	return nil
}

func (coordinator *Coordinator) stagedFromRecord(item ItemRecord) (StagedFile, error) {
	file, err := coordinator.store.OpenStaged(item.StagedPath)
	if err != nil {
		return StagedFile{}, err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil {
		return StagedFile{}, errors.Join(statErr, closeErr)
	}

	return StagedFile{Path: item.StagedPath, Digest: item.Digest, Size: info.Size()}, nil
}

func (coordinator *Coordinator) updateProgressForExisting(item ItemRecord) {
	coordinator.updateProgress(func(progress *Progress) {
		switch item.State {
		case ItemStateReady:
			progress.ItemsReady++
		case ItemStateDuplicate:
			progress.ItemsDuplicate++
		}
	})
}

func (coordinator *Coordinator) recordItemResult(result itemResult) {
	coordinator.updateProgress(func(progress *Progress) {
		switch result.state {
		case ItemStateReady:
			progress.ItemsReady++
		case ItemStateDuplicate:
			progress.ItemsDuplicate++
		case ItemStateBlocked:
			progress.ItemsBlocked++
		case ItemStateFailed:
			progress.ItemsFailed++
		}
	})
	if result.code != "" {
		coordinator.addFailure(ProgressFailure{
			Source: result.path.String(), Item: result.itemName,
			Code: result.code, Message: result.message,
		})
	}
}

func (coordinator *Coordinator) recordSourcePreparationFailure(
	candidate SourceCandidate,
	err error,
) {
	message := "source could not be prepared"
	code := ErrorCodeStorage
	if errors.Is(err, ErrSourceChanged) {
		message = "source bytes changed after this path was recorded; rename it to import the replacement"
		code = ErrorCodeSourceChanged
	}
	coordinator.addFailure(ProgressFailure{
		Source: candidate.Path.String(), Code: code, Message: message,
	})
	coordinator.updateProgress(func(progress *Progress) {
		progress.SourcesCompleted++
		progress.SourcesRetained++
	})
}

func (coordinator *Coordinator) completeProgress() {
	coordinator.updateProgress(func(progress *Progress) {
		progress.Active = false
		progress.CompletedAt = coordinator.now().UTC()
	})
}

func userSafeScanFailure(err error) string {
	switch {
	case errors.Is(err, ErrUnsafeArchive):
		return "archive contains an unsafe entry"
	case errors.Is(err, ErrArchiveLimit):
		return "archive exceeds configured safety limits"
	case errors.Is(err, ErrTruncatedArchive):
		return "archive contains truncated or unreadable data"
	case errors.Is(err, fs.ErrNotExist):
		return "source disappeared during processing"
	default:
		return "source could not be processed"
	}
}
