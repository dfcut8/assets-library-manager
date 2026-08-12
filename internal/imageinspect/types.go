// Package imageinspect validates untrusted images and creates bounded derived previews.
package imageinspect

import (
	"errors"
)

// Stable inspection errors let the coordinator classify unsafe item failures.
var (
	ErrUnsupportedFormat = errors.New("imageinspect: unsupported image format")
	ErrFormatMismatch    = errors.New("imageinspect: extension and content do not match")
	ErrSourceLimit       = errors.New("imageinspect: source byte limit exceeded")
	ErrPixelLimit        = errors.New("imageinspect: pixel limit exceeded")
	ErrInvalidMetadata   = errors.New("imageinspect: invalid image metadata")
	ErrDecoderPanic      = errors.New("imageinspect: decoder panic")
	ErrRenditionLimit    = errors.New("imageinspect: rendition byte limit exceeded")
)

// Limits bounds all inspection allocation and output work.
type Limits struct {
	MaxSourceBytes        int64
	MaxImagePixels        int64
	ThumbnailMaxDimension int
	AnalysisMaxDimension  int
	MaxAnalysisBytes      int64
	ExpectedExtension     string
}

// DominantColor is one deterministic sampled 5-bit RGB histogram bucket.
type DominantColor struct {
	Hex     string
	Samples uint64
}

// Rendition is one bounded transient or persisted derived image.
type Rendition struct {
	MIMEType  string
	Extension string
	Width     int
	Height    int
	Data      []byte
}

// Inspection contains validated technical metadata and derived previews.
type Inspection struct {
	Format            string
	MIMEType          string
	FileSizeBytes     int64
	EncodedWidth      int
	EncodedHeight     int
	DisplayWidth      int
	DisplayHeight     int
	AspectRatio       float64
	Orientation       int
	OrientationClass  string
	HasAlpha          bool
	HasTransparency   bool
	EncodedAnimated   bool
	EncodedFrameCount int
	DominantColors    []DominantColor
	Thumbnail         Rendition
	Analysis          Rendition
}

// Inspector performs stateless bounded image inspection.
type Inspector struct{}

// New returns a reusable stateless Inspector.
func New() Inspector { return Inspector{} }
