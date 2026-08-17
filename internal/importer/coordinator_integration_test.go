//go:build integration

package importer_test

import (
	"archive/zip"
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

func TestCoordinatorDeletesSuccessfullyProcessedLooseAndZIPSources(t *testing.T) {
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
	imageData, err := os.ReadFile(filepath.Join(paths.Incoming, "a.png"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestZIP(t, filepath.Join(paths.Incoming, "c.zip"), "nested/c.png", imageData)
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
	if progress.Active || progress.ItemsReady != 1 || progress.ItemsDuplicate != 2 ||
		progress.SourcesDeleted != 3 || progress.SourcesRetained != 0 {
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
	for _, name := range []string{"a.png", "b.PNG", "c.zip"} {
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

func TestCoordinatorDeletesReusedIncomingFilename(t *testing.T) {
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

	const sourceName = "reused.png"
	sourceFile := filepath.Join(paths.Incoming, sourceName)
	sourcePath, err := importer.NewSourcePath(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &acceptingAnalyzer{}
	runScan := func() importer.Progress {
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
		if _, err := os.Stat(sourceFile); !os.IsNotExist(err) {
			t.Fatalf("source still exists after scan: %v", err)
		}

		return coordinator.Snapshot()
	}

	writeTestPNGColor(t, sourceFile, color.NRGBA{B: 255, A: 255})
	firstDigest, _, err := store.FingerprintIncoming(ctx, sourcePath, cfg.Processing.MaxSourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	firstProgress := runScan()
	if firstProgress.ItemsReady != 1 || firstProgress.SourcesDeleted != 1 {
		t.Fatalf("first progress = %#v", firstProgress)
	}
	if got := analyzer.calls.Load(); got != 1 {
		t.Fatalf("Analyze calls after first source = %d, want 1", got)
	}

	writeTestPNGColor(t, sourceFile, color.NRGBA{B: 255, A: 255})
	repeatedProgress := runScan()
	if repeatedProgress.ItemsReady != 1 || repeatedProgress.SourcesDeleted != 1 {
		t.Fatalf("repeated progress = %#v", repeatedProgress)
	}
	if got := analyzer.calls.Load(); got != 1 {
		t.Fatalf("Analyze calls after identical source = %d, want 1", got)
	}

	writeTestPNGColor(t, sourceFile, color.NRGBA{R: 255, A: 255})
	replacementDigest, _, err := store.FingerprintIncoming(
		ctx, sourcePath, cfg.Processing.MaxSourceBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacementDigest == firstDigest {
		t.Fatal("replacement digest unexpectedly matched first source")
	}
	replacementProgress := runScan()
	if replacementProgress.ItemsReady != 1 || replacementProgress.SourcesDeleted != 1 {
		t.Fatalf("replacement progress = %#v", replacementProgress)
	}
	if got := analyzer.calls.Load(); got != 2 {
		t.Fatalf("Analyze calls after replacement source = %d, want 2", got)
	}
	for _, digest := range []importer.Digest{firstDigest, replacementDigest} {
		if _, err := database.FindReadyByDigest(ctx, digest); err != nil {
			t.Fatalf("FindReadyByDigest(%s) error = %v", digest, err)
		}
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
	writeTestPNGColor(t, filePath, color.NRGBA{B: 255, A: 255})
}

func writeTestPNGColor(t *testing.T, filePath string, pixel color.NRGBA) {
	t.Helper()
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	imageData := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			imageData.SetNRGBA(x, y, pixel)
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

func writeTestZIP(t *testing.T, filePath, entryName string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entry, err := archive.Create(entryName)
	if err == nil {
		_, err = entry.Write(data)
	}
	archiveCloseErr := archive.Close()
	fileCloseErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if archiveCloseErr != nil {
		t.Fatal(archiveCloseErr)
	}
	if fileCloseErr != nil {
		t.Fatal(fileCloseErr)
	}
}
