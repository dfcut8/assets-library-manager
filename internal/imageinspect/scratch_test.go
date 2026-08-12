package imageinspect

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestScratchStoreCreatesOnePrivateRenditionAndRemovesIt(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	store, err := OpenScratchStore(parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	itemID := "0123456789abcdef0123456789abcdef"
	want := []byte("bounded image payload")
	scratch, err := store.Create(context.Background(), itemID, Rendition{
		MIMEType: "image/png", Extension: ".png", Data: want,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(parent, scratch.Directory))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "analysis.png" {
		t.Fatalf("scratch entries = %v", entries)
	}
	got, err := os.ReadFile(scratch.AbsolutePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("scratch bytes = %q", got)
	}
	info, err := os.Stat(scratch.AbsolutePath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("scratch mode = %o", info.Mode().Perm())
	}
	if _, err := store.Create(context.Background(), itemID, Rendition{
		MIMEType: "image/jpeg", Extension: ".jpg", Data: []byte("second"),
	}); err == nil {
		t.Fatal("Create() replaced an existing item scratch directory")
	}
	if err := store.Remove(context.Background(), scratch); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, scratch.Directory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch directory remains: %v", err)
	}
}

func TestScratchStoreRejectsForgedAndCanceledOperations(t *testing.T) {
	t.Parallel()

	store, err := OpenScratchStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if err := store.Remove(context.Background(), ScratchFile{
		Directory: "../outside.scratch", RelativePath: "../outside.scratch/analysis.png",
	}); err == nil {
		t.Fatal("Remove() accepted a forged path")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	itemID := "abcdef0123456789abcdef0123456789"
	if _, err := store.Create(ctx, itemID, Rendition{
		MIMEType: "image/png", Extension: ".png", Data: []byte("payload"),
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create() error = %v", err)
	}
}
