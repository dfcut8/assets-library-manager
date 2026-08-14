package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dfcut8/assets-library-manager/internal/importer"
)

func TestStoreStagePromoteVerifyAndOpen(t *testing.T) {
	t.Parallel()

	store, paths := newTestStore(t)
	data := []byte("byte-identical original")
	itemID := mustID(t, "0123456789abcdef0123456789abcdef")
	staged, err := store.Stage(context.Background(), itemID, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	wantDigest := importer.NewDigest(sha256.Sum256(data))
	if staged.Digest != wantDigest || staged.Size != int64(len(data)) {
		t.Fatalf("Stage() = %+v", staged)
	}
	info, err := os.Stat(filepath.Join(paths.Staging, staged.Path.String()))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("staged mode = %o, want no group/world access", info.Mode().Perm())
	}

	managedPath := mustManagedPath(t, "other/original--0123456789ab.png")
	if err := store.Promote(context.Background(), staged, managedPath); err != nil {
		t.Fatalf("Promote() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Staging, staged.Path.String())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file remains after promotion: %v", err)
	}
	matches, err := store.VerifyManaged(context.Background(), managedPath, wantDigest, int64(len(data)))
	if err != nil || !matches {
		t.Fatalf("VerifyManaged() = %v, %v", matches, err)
	}
	file, err := store.OpenManaged(managedPath)
	if err != nil {
		t.Fatalf("OpenManaged() error = %v", err)
	}
	got, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("reading managed file: %v, closing: %v", readErr, closeErr)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("managed bytes = %q", got)
	}
}

func TestStoreSnapshotIncomingReturnsTopLevelEntryMetadata(t *testing.T) {
	t.Parallel()

	store, paths := newTestStore(t)
	filePath := filepath.Join(paths.Incoming, "asset.PNG")
	if err := os.WriteFile(filePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(paths.Incoming, "ignored"), 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.SnapshotIncoming(context.Background())
	if err != nil {
		t.Fatalf("SnapshotIncoming() error = %v", err)
	}
	if len(snapshot) != 2 {
		t.Fatalf("SnapshotIncoming() returned %d entries", len(snapshot))
	}
	byName := make(map[string]importer.IncomingEntry, len(snapshot))
	for _, entry := range snapshot {
		byName[entry.Name] = entry
	}
	if entry := byName["asset.PNG"]; !entry.Mode.IsRegular() || entry.Size != 5 {
		t.Fatalf("file entry = %+v", entry)
	}
	if entry := byName["ignored"]; !entry.Mode.IsDir() {
		t.Fatalf("directory entry = %+v", entry)
	}
}

func TestStoreStageLimitRemovesPartialFile(t *testing.T) {
	t.Parallel()

	store, paths := newTestStore(t)
	itemID := mustID(t, "11111111111111111111111111111111")
	if _, err := store.Stage(context.Background(), itemID, bytes.NewReader([]byte("too large")), 3); err == nil {
		t.Fatal("Stage() succeeded past byte limit")
	}
	stagedPath, err := importer.NewStagedPath(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.Staging, stagedPath.String())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial staging file remains: %v", err)
	}
}

func TestStoreVerifiesStagedAndOwnsPrivateAnalysisScratch(t *testing.T) {
	t.Parallel()

	store, paths := newTestStore(t)
	itemID := mustID(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	data := []byte("staged original")
	staged, err := store.Stage(context.Background(), itemID, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	matches, err := store.VerifyStaged(context.Background(), staged.Path, staged.Digest, staged.Size)
	if err != nil || !matches {
		t.Fatalf("VerifyStaged() = %v, %v", matches, err)
	}
	scratch, err := store.CreateAnalysisScratch(itemID, ".png", []byte("bounded rendition"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(scratch.Path) != scratch.Directory {
		t.Fatalf("scratch = %+v", scratch)
	}
	info, err := os.Stat(scratch.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("scratch mode = %o, want no group/world access", info.Mode().Perm())
	}
	entries, err := os.ReadDir(scratch.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "analysis.png" {
		t.Fatalf("scratch entries = %v", entries)
	}
	if err := store.RemoveAnalysisScratch(itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.Staging, itemID.String()+".scratch")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch directory remains: %v", err)
	}
}

func TestStorePromoteCollisionRequiresFullDigestMatch(t *testing.T) {
	t.Parallel()

	store, paths := newTestStore(t)
	destination := mustManagedPath(t, "other/collision.png")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(paths.Processed, filepath.FromSlash(destination.String()))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Processed, filepath.FromSlash(destination.String())), []byte("different"), 0o600); err != nil {
		t.Fatal(err)
	}
	itemID := mustID(t, "22222222222222222222222222222222")
	staged, err := store.Stage(context.Background(), itemID, bytes.NewReader([]byte("original")), 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Promote(context.Background(), staged, destination); !errors.Is(err, importer.ErrConflict) {
		t.Fatalf("Promote() error = %v, want conflict", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Staging, staged.Path.String())); err != nil {
		t.Fatalf("staged evidence was removed: %v", err)
	}
}

func TestStoreDeleteIncomingRevalidatesFingerprint(t *testing.T) {
	t.Parallel()

	store, paths := newTestStore(t)
	sourcePath := mustSourcePath(t, "image.png")
	absolute := filepath.Join(paths.Incoming, sourcePath.String())
	if err := os.WriteFile(absolute, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, _, err := store.FingerprintIncoming(context.Background(), sourcePath, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteIncoming(context.Background(), sourcePath, digest, 64); !errors.Is(err, importer.ErrSourceChanged) {
		t.Fatalf("DeleteIncoming() error = %v", err)
	}
	if _, err := os.Stat(absolute); err != nil {
		t.Fatalf("changed source was removed: %v", err)
	}
	current, _, err := store.FingerprintIncoming(context.Background(), sourcePath, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteIncoming(context.Background(), sourcePath, current, 64); err != nil {
		t.Fatalf("DeleteIncoming() error = %v", err)
	}
}

func TestStoreCleanStagingRemovesUnreferencedEntries(t *testing.T) {
	t.Parallel()

	store, paths := newTestStore(t)
	orphan := "33333333333333333333333333333333.stage"
	referenced := "44444444444444444444444444444444.stage"
	newFile := "55555555555555555555555555555555.stage"
	invalid := "notes.txt"
	for _, name := range []string{orphan, referenced, newFile, invalid} {
		absolute := filepath.Join(paths.Staging, name)
		if err := os.WriteFile(absolute, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{
		"66666666666666666666666666666666.scratch",
		"unexpected-directory",
		filepath.Join(".codex-analysis", "previous-run"),
	} {
		if err := os.MkdirAll(filepath.Join(paths.Staging, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	referencedPath, err := importer.ParseStagedPath(referenced)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CleanStaging(context.Background(), []importer.StagedPath{referencedPath}); err != nil {
		t.Fatalf("CleanStaging() error = %v", err)
	}
	entries, err := os.ReadDir(paths.Staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != ".codex-analysis" ||
		entries[1].Name() != referenced {
		t.Fatalf("staging entries = %v, want workspace and referenced file", entries)
	}
	workspaceEntries, err := os.ReadDir(filepath.Join(paths.Staging, ".codex-analysis"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaceEntries) != 0 {
		t.Fatalf("analysis workspace entries = %v, want empty", workspaceEntries)
	}
}

func newTestStore(t *testing.T) (*Store, Paths) {
	t.Helper()

	root := t.TempDir()
	paths := Paths{
		Incoming:  filepath.Join(root, "incoming"),
		Processed: filepath.Join(root, "processed"),
		Staging:   filepath.Join(root, "processed", ".staging"),
	}
	for _, directory := range []string{paths.Incoming, paths.Processed, paths.Staging} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	return store, paths
}

func mustID(t *testing.T, value string) importer.ID {
	t.Helper()
	id, err := importer.ParseID(value)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func mustManagedPath(t *testing.T, value string) importer.ManagedPath {
	t.Helper()
	managedPath, err := importer.NewManagedPath(value)
	if err != nil {
		t.Fatal(err)
	}

	return managedPath
}

func mustSourcePath(t *testing.T, value string) importer.SourcePath {
	t.Helper()
	sourcePath, err := importer.NewSourcePath(value)
	if err != nil {
		t.Fatal(err)
	}

	return sourcePath
}
