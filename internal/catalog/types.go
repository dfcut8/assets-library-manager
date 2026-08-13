package catalog

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultPageSize is used when callers omit a result-page size.
	DefaultPageSize = 48
	// MaximumPageSize bounds catalog query memory and response work.
	MaximumPageSize = 200
)

var (
	// ErrNotFound means no ready catalog asset matched the requested identifier.
	ErrNotFound = errors.New("catalog: asset not found")
	// ErrStaleEdit means the optimistic version no longer matches current metadata.
	ErrStaleEdit = errors.New("catalog: stale metadata edit")
	// ErrInvalid means catalog input failed validation.
	ErrInvalid = errors.New("catalog: invalid input")
)

// AssetID is a validated lower-case 128-bit hexadecimal catalog identifier.
type AssetID string

// ParseAssetID validates a catalog identifier at the request boundary.
func ParseAssetID(value string) (AssetID, error) {
	if len(value) != 32 {
		return "", errors.New("catalog: asset identifier must contain 32 hexadecimal characters")
	}
	for _, character := range value {
		isDigit := character >= '0' && character <= '9'
		isLowerHex := character >= 'a' && character <= 'f'
		if !isDigit && !isLowerHex {
			return "", errors.New("catalog: asset identifier must be lowercase hexadecimal")
		}
	}

	return AssetID(value), nil
}

// String returns the persisted catalog identifier.
func (id AssetID) String() string { return string(id) }

// Sort is an allowlisted catalog ordering.
type Sort string

const (
	// SortRelevance orders FTS matches by rank and stable import identity.
	SortRelevance Sort = "relevance"
	// SortNewest orders assets by descending import time.
	SortNewest Sort = "newest"
	// SortOldest orders assets by ascending import time.
	SortOldest Sort = "oldest"
	// SortTitle orders assets by case-insensitive title.
	SortTitle Sort = "title"
	// SortWidth orders assets by descending display width.
	SortWidth Sort = "width"
	// SortHeight orders assets by descending display height.
	SortHeight Sort = "height"
	// SortFileSize orders assets by descending original byte length.
	SortFileSize Sort = "file-size"
)

// Valid reports whether a sort value is safe and supported.
func (sort Sort) Valid() bool {
	switch sort {
	case SortRelevance, SortNewest, SortOldest, SortTitle, SortWidth, SortHeight, SortFileSize:
		return true
	default:
		return false
	}
}

// TagFilter is one facet-qualified structured filter.
type TagFilter struct {
	Facet string
	Slug  string
}

// AssetQuery contains normalized free-text, pagination, sort, and structured filters.
type AssetQuery struct {
	Q            string
	Page         int
	PageSize     int
	Sort         Sort
	Types        []PrimaryType
	Styles       []string
	Orientations []string
	Formats      []string
	Tags         []TagFilter
	PixelArt     *bool
	Transparency *bool
	Animated     *bool
	MinWidth     int
	MaxWidth     int
	MinHeight    int
	MaxHeight    int
	ImportedFrom *time.Time
	ImportedTo   *time.Time
}

// Page is one deterministic result page.
type Page[T any] struct {
	Items      []T
	Page       int
	PageSize   int
	TotalItems int
	TotalPages int
}

// Tag is editable, facet-qualified catalog metadata.
type Tag struct {
	Facet  string
	Slug   string
	Label  string
	Origin string
}

// AssetSummary contains catalog-grid metadata without thumbnail bytes.
type AssetSummary struct {
	ID              AssetID
	Title           string
	PrimaryType     PrimaryType
	Style           string
	DisplayWidth    int
	DisplayHeight   int
	Format          string
	PixelArt        bool
	HasTransparency bool
	EncodedAnimated bool
	ImportedAt      time.Time
	UpdatedAt       time.Time
	Version         int
	Tags            []Tag
}

// DominantColor is one locally extracted palette entry.
type DominantColor struct {
	Hex     string `json:"hex"`
	Samples uint64 `json:"samples"`
}

// AIProvenance is the latest immutable semantic-analysis run for an asset.
type AIProvenance struct {
	Provider        string
	Model           string
	ReasoningEffort string
	ImageDetail     string
	PromptVersion   string
	SchemaVersion   string
	AttemptNumber   int
	Outcome         string
	Latency         time.Duration
	RequestID       string
	StartedAt       time.Time
	CompletedAt     *time.Time
}

// AssetDetail contains ready technical, semantic, layout, and provenance metadata.
type AssetDetail struct {
	AssetSummary
	SHA256            string
	OriginalFilename  string
	ManagedPath       string
	MIMEType          string
	FileSizeBytes     int64
	OrientationClass  string
	HasAlpha          bool
	EncodedFrameCount int
	DominantColors    []DominantColor
	Description       string
	AIConfidence      float64
	Layout            Layout
	Thumbnail         Thumbnail
	AI                *AIProvenance
}

// Thumbnail is a ready asset's bounded PNG preview.
type Thumbnail struct {
	MIMEType  string
	Width     int
	Height    int
	Data      []byte
	Version   int
	UpdatedAt time.Time
}

// Original identifies a ready managed file without opening it.
type Original struct {
	ID               AssetID
	ManagedPath      string
	OriginalFilename string
	MIMEType         string
	FileSizeBytes    int64
	SHA256           string
	UpdatedAt        time.Time
}

// MetadataEdit replaces current editable semantic metadata at an optimistic version.
type MetadataEdit struct {
	Version     int
	Title       string
	Description string
	PrimaryType PrimaryType
	Style       string
	PixelArt    bool
	Layout      Layout
	Tags        []Tag
}

// StaleEditError carries the submitted and authoritative versions.
type StaleEditError struct {
	SubmittedVersion int
	CurrentVersion   int
}

func (err *StaleEditError) Error() string {
	return fmt.Sprintf(
		"catalog: stale metadata edit: submitted version %d, current version %d",
		err.SubmittedVersion,
		err.CurrentVersion,
	)
}

// Unwrap supports errors.Is(err, ErrStaleEdit).
func (*StaleEditError) Unwrap() error { return ErrStaleEdit }

// ValidationError exposes bounded field-specific failures to HTML form handlers.
type ValidationError struct {
	Fields map[string]string
}

func (err *ValidationError) Error() string {
	if err == nil || len(err.Fields) == 0 {
		return ErrInvalid.Error()
	}

	return ErrInvalid.Error() + ": one or more fields are invalid"
}

// Unwrap supports errors.Is(err, ErrInvalid).
func (*ValidationError) Unwrap() error { return ErrInvalid }

func validationError(field, message string) *ValidationError {
	return &ValidationError{Fields: map[string]string{field: strings.TrimSpace(message)}}
}
