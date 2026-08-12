//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBinaryVersionAndSafeMissingDatabaseExit(t *testing.T) {
	root := t.TempDir()
	binaryName := "asset-library-manager"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(root, binaryName)
	build := exec.Command("go", "build", "-trimpath", "-o", binaryPath, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building test binary: %v\n%s", err, output)
	}

	version := exec.Command(binaryPath, "--version")
	if output, err := version.CombinedOutput(); err != nil || !bytes.Contains(output, []byte("asset-library-manager")) {
		t.Fatalf("--version: error=%v output=%q", err, output)
	}
	if _, err := os.Stat(filepath.Join(root, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--version created runtime state: %v", err)
	}

	managedPath := filepath.Join(root, "processed", "vehicle", "retained.png")
	if err := os.MkdirAll(filepath.Dir(managedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("retained")
	if err := os.WriteFile(managedPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("conflict startup: error=%v output=%q", err, output)
	}
	if !bytes.Contains(output, []byte("catalog metadata may have been lost")) ||
		!bytes.Contains(output, []byte("restore assets.db or move")) {
		t.Fatalf("startup guidance = %q", output)
	}
	if _, statErr := os.Stat(filepath.Join(root, "assets.db")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("conflict startup created a database: %v", statErr)
	}
	got, readErr := os.ReadFile(managedPath)
	if readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("conflict startup changed managed data: got=%q error=%v", got, readErr)
	}
}
