// Package importer defines the durable import workflow and its storage contracts.
package importer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
)

const (
	idBytes          = 16
	maxRelativePath  = 1024
	maxOriginalName  = 255
	maxErrorMessage  = 2048
	stagedFileSuffix = ".stage"
)

var (
	stagedNamePattern = regexp.MustCompile(`^[0-9a-f]{32}\.stage$`)
	tagLabelPattern   = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,128}$`)
)

// Expected workflow errors are stable and inspectable across adapters.
var (
	ErrNotFound          = errors.New("importer: not found")
	ErrConflict          = errors.New("importer: conflict")
	ErrInvalidTransition = errors.New("importer: invalid state transition")
	ErrSourceChanged     = errors.New("importer: source changed")
)

// ID is a cryptographically random 128-bit workflow identifier.
type ID [idBytes]byte

// NewID returns a new identifier using crypto/rand.
func NewID() (ID, error) {
	var id ID
	if _, err := rand.Read(id[:]); err != nil {
		return ID{}, fmt.Errorf("generating import identifier: %w", err)
	}

	return id, nil
}

// ParseID validates a lower-case, 32-character hexadecimal identifier.
func ParseID(value string) (ID, error) {
	var id ID
	if len(value) != hex.EncodedLen(len(id)) || value != strings.ToLower(value) {
		return ID{}, errors.New("importer: identifier must be 32 lowercase hexadecimal characters")
	}
	if _, err := hex.Decode(id[:], []byte(value)); err != nil {
		return ID{}, fmt.Errorf("importer: decoding identifier: %w", err)
	}

	return id, nil
}

// String returns the lower-case hexadecimal representation used by SQLite.
func (id ID) String() string {
	return hex.EncodeToString(id[:])
}

// IsZero reports whether the identifier has not been set.
func (id ID) IsZero() bool {
	return id == ID{}
}

// Digest is a complete SHA-256 digest.
type Digest [sha256.Size]byte

// ParseDigest validates a lower-case hexadecimal SHA-256 digest.
func ParseDigest(value string) (Digest, error) {
	var digest Digest
	if len(value) != hex.EncodedLen(len(digest)) || value != strings.ToLower(value) {
		return Digest{}, errors.New("importer: digest must be 64 lowercase hexadecimal characters")
	}
	if _, err := hex.Decode(digest[:], []byte(value)); err != nil {
		return Digest{}, fmt.Errorf("importer: decoding digest: %w", err)
	}

	return digest, nil
}

// NewDigest converts a standard-library SHA-256 sum without allocation.
func NewDigest(sum [sha256.Size]byte) Digest {
	return Digest(sum)
}

// String returns the lower-case hexadecimal digest.
func (digest Digest) String() string {
	return hex.EncodeToString(digest[:])
}

// Bytes returns a defensive copy suitable for database parameters.
func (digest Digest) Bytes() []byte {
	data := make([]byte, len(digest))
	copy(data, digest[:])

	return data
}

// SourcePath is a validated path relative to incoming/.
type SourcePath string

// NewSourcePath validates an incoming-directory-relative path.
func NewSourcePath(value string) (SourcePath, error) {
	if err := validateRelativePath(value, false); err != nil {
		return "", fmt.Errorf("importer: invalid source path: %w", err)
	}

	return SourcePath(value), nil
}

// String returns the slash-separated relative path.
func (value SourcePath) String() string { return string(value) }

// StagedPath is a validated staging filename.
type StagedPath string

// NewStagedPath derives the only accepted staging filename for an item.
func NewStagedPath(itemID ID) (StagedPath, error) {
	if itemID.IsZero() {
		return "", errors.New("importer: staged path requires a non-zero item identifier")
	}

	return StagedPath(itemID.String() + stagedFileSuffix), nil
}

// ParseStagedPath validates a persisted staging filename.
func ParseStagedPath(value string) (StagedPath, error) {
	if !stagedNamePattern.MatchString(value) {
		return "", errors.New("importer: staged path must match <item-id>.stage")
	}

	return StagedPath(value), nil
}

// String returns the staging-root-relative filename.
func (value StagedPath) String() string { return string(value) }

// ManagedPath is a validated relative path beneath processed/.
type ManagedPath string

// NewManagedPath validates a persisted managed relative path.
func NewManagedPath(value string) (ManagedPath, error) {
	if err := validateRelativePath(value, true); err != nil {
		return "", fmt.Errorf("importer: invalid managed path: %w", err)
	}
	if value == ".staging" || strings.HasPrefix(value, ".staging/") {
		return "", errors.New("importer: managed path must not use the staging namespace")
	}

	return ManagedPath(value), nil
}

// String returns the processed-root-relative path.
func (value ManagedPath) String() string { return string(value) }

func validateRelativePath(value string, allowNested bool) error {
	if value == "" || value == "." || len(value) > maxRelativePath {
		return errors.New("path must be non-empty and at most 1024 bytes")
	}
	if strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value {
		return errors.New("path must be normalized, relative, and slash-separated")
	}
	if value == ".." || strings.HasPrefix(value, "../") || hasDrivePrefix(value) {
		return errors.New("path escapes its managed root")
	}
	if !allowNested && strings.ContainsRune(value, '/') {
		return errors.New("nested incoming source paths are not supported")
	}

	return nil
}

func hasDrivePrefix(value string) bool {
	return len(value) >= 2 && value[1] == ':' &&
		((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z'))
}

// SourceType describes the physical incoming source.
type SourceType string

// Supported source types, including the invalid zero value.
const (
	SourceTypeUnknown SourceType = ""
	SourceTypeLoose   SourceType = "loose"
	SourceTypeZIP     SourceType = "zip"
)

// Valid reports whether the source type can be persisted.
func (value SourceType) Valid() bool {
	return value == SourceTypeLoose || value == SourceTypeZIP
}

// ItemState is the durable per-image workflow state.
type ItemState string

// Item workflow states, including the invalid zero value.
const (
	ItemStateUnknown    ItemState = ""
	ItemStateDiscovered ItemState = "discovered"
	ItemStateStaged     ItemState = "staged"
	ItemStateAnalyzing  ItemState = "analyzing"
	ItemStateCommitting ItemState = "committing"
	ItemStateReady      ItemState = "ready"
	ItemStateDuplicate  ItemState = "duplicate"
	ItemStateBlocked    ItemState = "blocked"
	ItemStateFailed     ItemState = "failed"
)

// Valid reports whether the item state can be persisted.
func (state ItemState) Valid() bool {
	switch state {
	case ItemStateDiscovered, ItemStateStaged, ItemStateAnalyzing, ItemStateCommitting,
		ItemStateReady, ItemStateDuplicate, ItemStateBlocked, ItemStateFailed:
		return true
	default:
		return false
	}
}

// Terminal reports whether no later item transition is permitted.
func (state ItemState) Terminal() bool {
	return state == ItemStateReady || state == ItemStateDuplicate
}

// CanTransitionTo reports whether a state change follows the normative workflow.
func (state ItemState) CanTransitionTo(next ItemState) bool {
	switch state {
	case ItemStateDiscovered:
		return next == ItemStateStaged || next == ItemStateDuplicate ||
			next == ItemStateBlocked || next == ItemStateFailed
	case ItemStateStaged:
		return next == ItemStateAnalyzing || next == ItemStateDuplicate || next == ItemStateFailed
	case ItemStateAnalyzing:
		return next == ItemStateCommitting || next == ItemStateFailed
	case ItemStateCommitting:
		return next == ItemStateReady || next == ItemStateFailed
	case ItemStateBlocked, ItemStateFailed:
		return next == ItemStateDiscovered
	default:
		return false
	}
}

// SourceState is the durable aggregate source state.
type SourceState string

// Source aggregation states, including the invalid zero value.
const (
	SourceStateUnknown    SourceState = ""
	SourceStateDiscovered SourceState = "discovered"
	SourceStateProcessing SourceState = "processing"
	SourceStateReady      SourceState = "ready"
	SourceStateDuplicate  SourceState = "duplicate"
	SourceStateBlocked    SourceState = "blocked"
	SourceStateFailed     SourceState = "failed"
	SourceStateRetained   SourceState = "retained"
	SourceStateDeleted    SourceState = "deleted"
)

// Valid reports whether the source state can be persisted.
func (state SourceState) Valid() bool {
	switch state {
	case SourceStateDiscovered, SourceStateProcessing, SourceStateReady, SourceStateDuplicate,
		SourceStateBlocked, SourceStateFailed, SourceStateRetained, SourceStateDeleted:
		return true
	default:
		return false
	}
}

// DeletionState is the durable source-deletion workflow state.
type DeletionState string

// Source deletion states, including the invalid zero value.
const (
	DeletionStateUnknown     DeletionState = ""
	DeletionStateNotEligible DeletionState = "not-eligible"
	DeletionStateEligible    DeletionState = "eligible"
	DeletionStatePending     DeletionState = "pending"
	DeletionStateDeleted     DeletionState = "deleted"
	DeletionStateFailed      DeletionState = "failed"
)

// Valid reports whether the deletion state can be persisted.
func (state DeletionState) Valid() bool {
	switch state {
	case DeletionStateNotEligible, DeletionStateEligible, DeletionStatePending,
		DeletionStateDeleted, DeletionStateFailed:
		return true
	default:
		return false
	}
}

// AssetState describes filesystem/catalog consistency.
type AssetState string

// Asset persistence states, including the invalid zero value.
const (
	AssetStateUnknown         AssetState = ""
	AssetStateStaged          AssetState = "staged"
	AssetStateReady           AssetState = "ready"
	AssetStateIntegrityFailed AssetState = "integrity-failed"
)

// Valid reports whether the asset state can be persisted.
func (state AssetState) Valid() bool {
	return state == AssetStateStaged || state == AssetStateReady || state == AssetStateIntegrityFailed
}

// ErrorCode is a stable machine-readable import failure classification.
type ErrorCode string

// Stable processing error codes persisted for user-safe reporting.
const (
	ErrorCodeSourceChanged    ErrorCode = "source_changed"
	ErrorCodeInvalidInput     ErrorCode = "invalid_input"
	ErrorCodeStorage          ErrorCode = "storage_error"
	ErrorCodeIntegrity        ErrorCode = "integrity_failed"
	ErrorCodeCodexUnavailable ErrorCode = "codex_unavailable"
	ErrorCodeInternal         ErrorCode = "internal_error"
)

// Valid reports whether the error code is part of the stable processing vocabulary.
func (code ErrorCode) Valid() bool {
	switch code {
	case ErrorCodeSourceChanged, ErrorCodeInvalidInput, ErrorCodeStorage, ErrorCodeIntegrity,
		ErrorCodeCodexUnavailable, ErrorCodeInternal:
		return true
	default:
		return false
	}
}

// SourceRecord is the persisted aggregate state for one incoming source.
type SourceRecord struct {
	ID                   ID
	Path                 SourcePath
	Type                 SourceType
	DiscoveryFingerprint Digest
	State                SourceState
	DeletionState        DeletionState
	RetainedReason       string
	ErrorCode            ErrorCode
	ErrorMessage         string
	DiscoveredAt         time.Time
	UpdatedAt            time.Time
}

// ItemRecord is the persisted workflow state for one supported image.
type ItemRecord struct {
	ID           ID
	SourceID     ID
	ZIPEntryName string
	StagedPath   StagedPath
	Digest       Digest
	AssetID      ID
	State        ItemState
	AttemptCount int
	ErrorCode    ErrorCode
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ItemTransition is one compare-and-swap workflow state change.
type ItemTransition struct {
	ID           ID
	From         ItemState
	To           ItemState
	StagedPath   StagedPath
	Digest       Digest
	AssetID      ID
	ErrorCode    ErrorCode
	ErrorMessage string
	UpdatedAt    time.Time
}

// SourceTransition is one compare-and-swap aggregate source state change.
type SourceTransition struct {
	ID             ID
	From           SourceState
	To             SourceState
	DeletionState  DeletionState
	RetainedReason string
	ErrorCode      ErrorCode
	ErrorMessage   string
	UpdatedAt      time.Time
}

// AssetRef is the minimal ready-asset identity needed for deduplication.
type AssetRef struct {
	ID          ID
	Digest      Digest
	ManagedPath ManagedPath
}

// StagedFile is a fully synced original awaiting promotion.
type StagedFile struct {
	Path   StagedPath
	Digest Digest
	Size   int64
}

// ScratchImage identifies the sole transient analysis file in an item-specific directory.
type ScratchImage struct {
	Path      string
	Directory string
}

// Thumbnail is the validated PNG preview persisted with an asset.
type Thumbnail struct {
	Width  int
	Height int
	Data   []byte
}

// Tag is normalized semantic or deterministic catalog metadata.
type Tag struct {
	Facet  string
	Slug   string
	Label  string
	Origin string
}

// TokenUsage is the bounded model-token usage for one or more semantic-analysis attempts.
type TokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
}

// Add accumulates another token-usage measurement.
func (usage *TokenUsage) Add(other TokenUsage) {
	usage.InputTokens += other.InputTokens
	usage.CachedInputTokens += other.CachedInputTokens
	usage.CacheWriteInputTokens += other.CacheWriteInputTokens
	usage.OutputTokens += other.OutputTokens
	usage.ReasoningOutputTokens += other.ReasoningOutputTokens
	usage.TotalTokens += other.TotalTokens
}

// AIRun records one immutable semantic-analysis attempt.
type AIRun struct {
	ID                   ID
	Provider             string
	Model                string
	ReasoningEffort      string
	ImageDetail          string
	PromptVersion        string
	SchemaVersion        string
	AttemptNumber        int
	StartedAt            time.Time
	CompletedAt          *time.Time
	Latency              time.Duration
	RequestID            string
	UsageJSON            string
	Outcome              string
	ErrorCode            ErrorCode
	ErrorMessage         string
	NormalizedResultJSON string
}

// ImageInput identifies one bounded rendition and the durable item it belongs to.
type ImageInput struct {
	ItemID           ID
	AssetID          ID
	Path             string
	ScratchDirectory string
	DisplayWidth     int
	DisplayHeight    int
}

// AnalysisResult is normalized semantic catalog metadata.
type AnalysisResult struct {
	Title       string
	Description string
	PrimaryType catalog.PrimaryType
	Layout      catalog.Layout
	Style       string
	PixelArt    bool
	Confidence  float64
	Tags        []Tag
}

// AnalysisProvenance is the accepted immutable AI attempt committed with the asset.
type AnalysisProvenance struct {
	Run        AIRun
	TokenUsage TokenUsage
}

// StagedAsset contains all data committed before filesystem promotion.
type StagedAsset struct {
	ID                 ID
	ItemID             ID
	Digest             Digest
	OriginalFilename   string
	ManagedPath        ManagedPath
	Format             string
	MIMEType           string
	FileSizeBytes      int64
	DisplayWidth       int
	DisplayHeight      int
	OrientationClass   string
	HasAlpha           bool
	HasTransparency    bool
	EncodedAnimated    bool
	EncodedFrameCount  int
	DominantColorsJSON string
	Title              string
	Description        string
	PrimaryType        catalog.PrimaryType
	Style              string
	PixelArt           bool
	AIConfidence       float64
	Layout             catalog.Layout
	SearchTags         string
	Thumbnail          Thumbnail
	Tags               []Tag
	AIRun              AIRun
	CreatedAt          time.Time
}

// RecoveryAsset identifies an asset whose database state must be reconciled with files.
type RecoveryAsset struct {
	ID          ID
	ItemID      ID
	Digest      Digest
	Size        int64
	StagedPath  StagedPath
	ManagedPath ManagedPath
	State       AssetState
}

// PendingDeletion identifies a source whose eligible deletion did not complete.
type PendingDeletion struct {
	ID          ID
	Path        SourcePath
	Fingerprint Digest
	SourceState SourceState
	State       DeletionState
}

// SourceSummary is the authoritative item-state aggregation for one source.
type SourceSummary struct {
	SourceID   ID
	Total      int
	Discovered int
	Staged     int
	Analyzing  int
	Committing int
	Ready      int
	Duplicate  int
	Blocked    int
	Failed     int
}

// ValidateErrorFields applies the database's bounded diagnostic contract.
func ValidateErrorFields(code ErrorCode, message string) error {
	if code != "" && !code.Valid() {
		return errors.New("importer: error code is unsupported")
	}
	if len(code) > 128 {
		return errors.New("importer: error code is too long")
	}
	if len(message) > maxErrorMessage {
		return errors.New("importer: error message is too long")
	}

	return nil
}

// ValidateTag checks fields not already covered by catalog.ValidateTag.
func ValidateTag(tag Tag) error {
	if tag.Facet == "" || len(tag.Facet) > 64 {
		return errors.New("importer: tag facet must contain 1 to 64 characters")
	}
	if err := catalog.ValidateTag(tag.Slug); err != nil {
		return err
	}
	if !tagLabelPattern.MatchString(tag.Label) {
		return errors.New("importer: tag label must contain 1 to 128 printable characters")
	}
	switch tag.Origin {
	case "ai", "deterministic", "user":
		return nil
	default:
		return errors.New("importer: tag origin is unsupported")
	}
}

// ValidateOriginalFilename checks the database and user-interface boundary.
func ValidateOriginalFilename(value string) error {
	if value == "" || len(value) > maxOriginalName || strings.ContainsRune(value, '\x00') {
		return errors.New("importer: original filename must contain 1 to 255 bytes without NUL")
	}

	return nil
}
