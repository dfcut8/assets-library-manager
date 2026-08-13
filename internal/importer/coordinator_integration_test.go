//go:build integration

package importer_test

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/config"
	"github.com/dfcut8/assets-library-manager/internal/imageinspect"
	"github.com/dfcut8/assets-library-manager/internal/importer"
	"github.com/dfcut8/assets-library-manager/internal/sqlite"
	"github.com/dfcut8/assets-library-manager/internal/storage"
)

func TestCoordinatorImportsAndDeduplicatesIdenticalLooseImages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	cfg := config.Default()
	paths, err := storage.Prepare(root, cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sqlite.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})

	writeTestPNG(t, filepath.Join(paths.Incoming, "a.png"))
	writeTestPNG(t, filepath.Join(paths.Incoming, "b.PNG"))
	sourcePath, err := importer.NewSourcePath("a.png")
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := store.FingerprintIncoming(ctx, sourcePath, cfg.Processing.MaxSourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &acceptingAnalyzer{}
	coordinator, err := importer.NewCoordinator(
		coordinatorConfig(cfg), database, store, imageinspect.New(), analyzer, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Run(ctx); err != nil {
		t.Fatal(err)
	}

	progress := coordinator.Snapshot()
	if progress.Active || progress.ItemsReady != 1 || progress.ItemsDuplicate != 1 ||
		progress.SourcesDeleted != 2 || progress.SourcesRetained != 0 {
		t.Fatalf("progress = %#v", progress)
	}
	if got := analyzer.calls.Load(); got != 1 {
		t.Fatalf("Analyze calls = %d, want 1", got)
	}
	asset, err := database.FindReadyByDigest(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := store.VerifyManaged(ctx, asset.ManagedPath, digest, size)
	if err != nil || !matches {
		t.Fatalf("managed verification = %v, %v", matches, err)
	}
	for _, name := range []string{"a.png", "b.PNG"} {
		if _, err := os.Stat(filepath.Join(paths.Incoming, name)); !os.IsNotExist(err) {
			t.Fatalf("source %q still exists: %v", name, err)
		}
	}
	managedFile := filepath.Join(paths.Processed, filepath.FromSlash(asset.ManagedPath.String()))
	if err := os.WriteFile(managedFile, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery, err := importer.NewCoordinator(
		coordinatorConfig(cfg), database, store, imageinspect.New(), nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.FindReadyByDigest(ctx, digest); !errors.Is(err, importer.ErrNotFound) {
		t.Fatalf("corrupt ready asset remained visible: %v", err)
	}
}

type acceptingAnalyzer struct {
	calls atomic.Int32
}

func (analyzer *acceptingAnalyzer) Analyze(
	_ context.Context,
	input importer.ImageInput,
) (importer.AnalysisResult, importer.AnalysisProvenance, error) {
	analyzer.calls.Add(1)
	runID, err := importer.NewID()
	if err != nil {
		return importer.AnalysisResult{}, importer.AnalysisProvenance{}, err
	}
	started := time.Now().UTC()
	completed := started.Add(time.Millisecond)

	return importer.AnalysisResult{
			Title: "Blue test asset", Description: "A small blue square used for integration testing.",
			PrimaryType: catalog.PrimaryTypeProp,
			Layout:      catalog.Layout{Kind: catalog.LayoutKindSingle},
			Style:       "minimal", Confidence: 0.98,
			Tags: []importer.Tag{{Facet: "subject", Slug: "blue-square", Label: "Blue square", Origin: "ai"}},
		}, importer.AnalysisProvenance{Run: importer.AIRun{
			ID: runID, Provider: "openai", Model: "test-model", ReasoningEffort: "medium",
			ImageDetail: "auto", PromptVersion: "test-v1", SchemaVersion: "test-v1",
			AttemptNumber: 1, StartedAt: started, CompletedAt: &completed,
			Latency: time.Millisecond, Outcome: "accepted", NormalizedResultJSON: `{}`,
		}}, nil
}

func coordinatorConfig(cfg config.Config) importer.CoordinatorConfig {
	return importer.CoordinatorConfig{
		Workers: cfg.Processing.Workers, MaxSourceBytes: cfg.Processing.MaxSourceBytes,
		InspectionLimits: imageinspect.Limits{
			MaxSourceBytes:        cfg.Processing.MaxSourceBytes,
			MaxImagePixels:        cfg.Processing.MaxImagePixels,
			ThumbnailMaxDimension: cfg.Processing.ThumbnailMaxDimension,
			AnalysisMaxDimension:  cfg.Processing.AnalysisMaxDimension,
			MaxAnalysisBytes:      cfg.Processing.MaxAnalysisBytes,
		},
		ArchiveLimits: importer.ArchiveLimits{
			MaxEntries:                cfg.Processing.Archive.MaxEntries,
			MaxEntryBytes:             cfg.Processing.Archive.MaxEntryBytes,
			MaxTotalUncompressedBytes: cfg.Processing.Archive.MaxTotalUncompressedBytes,
			MaxCompressionRatio:       cfg.Processing.Archive.MaxCompressionRatio,
		},
	}
}

func writeTestPNG(t *testing.T, filePath string) {
	t.Helper()
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	imageData := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			imageData.SetNRGBA(x, y, color.NRGBA{B: 255, A: 255})
		}
	}
	encodeErr := png.Encode(file, imageData)
	closeErr := file.Close()
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}
