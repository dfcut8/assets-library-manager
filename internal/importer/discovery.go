package importer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// CandidateKind classifies a top-level incoming regular file by extension.
type CandidateKind string

// Incoming candidate classifications.
const (
	CandidateKindLoose       CandidateKind = "loose-image"
	CandidateKindZIP         CandidateKind = "zip"
	CandidateKindUnsupported CandidateKind = "unsupported"
)

// IncomingEntry is root-scoped directory metadata captured without following links.
type IncomingEntry struct {
	Name    string
	Mode    fs.FileMode
	Size    int64
	ModTime time.Time
}

// SourceCandidate is one immutable regular-file entry from the startup snapshot.
type SourceCandidate struct {
	Path      SourcePath
	Kind      CandidateKind
	Extension string
	Size      int64
	ModTime   time.Time
}

// DiscoveryFailure is an entry whose name cannot be represented safely.
type DiscoveryFailure struct {
	Name    string
	Message string
}

// Discovery is the complete startup-only incoming snapshot.
type Discovery struct {
	Sources  []SourceCandidate
	Failures []DiscoveryFailure
}

// IncomingSnapshotter is implemented by root-scoped storage beside this consumer.
type IncomingSnapshotter interface {
	SnapshotIncoming(context.Context) ([]IncomingEntry, error)
}

// Discover snapshots and deterministically classifies top-level incoming files.
func Discover(ctx context.Context, source IncomingSnapshotter) (Discovery, error) {
	if source == nil {
		return Discovery{}, errors.New("discovering incoming sources: snapshotter is nil")
	}
	if err := ctx.Err(); err != nil {
		return Discovery{}, fmt.Errorf("discovering incoming sources: %w", err)
	}
	entries, err := source.SnapshotIncoming(ctx)
	if err != nil {
		return Discovery{}, fmt.Errorf("discovering incoming sources: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Discovery{}, fmt.Errorf("discovering incoming sources: %w", err)
	}
	slices.SortStableFunc(entries, compareIncomingEntries)

	discovery := Discovery{
		Sources:  make([]SourceCandidate, 0, len(entries)),
		Failures: make([]DiscoveryFailure, 0),
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Discovery{}, fmt.Errorf("discovering incoming sources: %w", err)
		}
		if !entry.Mode.IsRegular() {
			continue
		}
		sourcePath, err := NewSourcePath(entry.Name)
		if err != nil {
			discovery.Failures = append(discovery.Failures, DiscoveryFailure{
				Name:    sanitizeDiscoveryName(entry.Name),
				Message: "incoming filename is not a safe relative path",
			})

			continue
		}
		extension := strings.ToLower(path.Ext(entry.Name))
		discovery.Sources = append(discovery.Sources, SourceCandidate{
			Path:      sourcePath,
			Kind:      classifyExtension(extension),
			Extension: extension,
			Size:      entry.Size,
			ModTime:   entry.ModTime,
		})
	}

	return discovery, nil
}

func compareIncomingEntries(left, right IncomingEntry) int {
	leftNormalized := strings.ToLower(left.Name)
	rightNormalized := strings.ToLower(right.Name)
	if comparison := strings.Compare(leftNormalized, rightNormalized); comparison != 0 {
		return comparison
	}

	return strings.Compare(left.Name, right.Name)
}

func classifyExtension(extension string) CandidateKind {
	switch extension {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return CandidateKindLoose
	case ".zip":
		return CandidateKindZIP
	default:
		return CandidateKindUnsupported
	}
}

func sanitizeDiscoveryName(value string) string {
	const maximum = 255
	var builder strings.Builder
	for _, character := range value {
		if character < ' ' || character == 0x7f {
			builder.WriteRune('\uFFFD')
		} else {
			builder.WriteRune(character)
		}
		if builder.Len() >= maximum {
			break
		}
	}

	return builder.String()
}
