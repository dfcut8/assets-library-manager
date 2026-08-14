//go:build integration && codex_integration

package codex

import (
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/importer"
)

func TestAnalyzer_AnalyzeInstalledCodex(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex command is not installed on PATH")
	}

	workingDirectory := t.TempDir()
	imagePath := filepath.Join(workingDirectory, "probe.png")
	writeIntegrationPNG(t, imagePath)
	itemID, err := importer.NewID()
	if err != nil {
		t.Fatal(err)
	}
	analyzer, err := StartAnalyzer(t.Context(), AnalyzerConfig{
		Command: "codex", Model: "gpt-5.6-luna", WorkingDirectory: workingDirectory,
		ReasoningEffort: "medium", TurnTimeout: 90 * time.Second,
		MaxAttempts: 1, InitialRetryDelay: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, &recordingAttempts{})
	if err != nil {
		t.Fatalf("StartAnalyzer() error = %v", err)
	}
	defer closeAnalyzer(t, analyzer)

	result, provenance, err := analyzer.Analyze(t.Context(), ImageInput{
		ItemID: itemID, Path: imagePath, ScratchDirectory: workingDirectory,
		DisplayWidth: 128, DisplayHeight: 128,
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if result.Title == "" || provenance.Run.Outcome != "accepted" {
		t.Fatalf("Analyze() result = %#v, provenance = %#v", result, provenance)
	}
}

func writeIntegrationPNG(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	imageData := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := range 128 {
		for x := range 128 {
			imageData.SetRGBA(x, y, color.RGBA{R: uint8(x * 2), G: uint8(y * 2), B: 96, A: 255})
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
