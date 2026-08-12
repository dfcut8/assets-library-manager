package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dfcut8/assets-library-manager/internal/config"
)

func TestPrepareAllowsSafeMissingDatabaseCases(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"processed absent": func(*testing.T, string) {},
		"processed empty": func(t *testing.T, root string) {
			t.Helper()
			mustMkdir(t, filepath.Join(root, "processed"))
		},
		"empty staging": func(t *testing.T, root string) {
			t.Helper()
			mustMkdir(t, filepath.Join(root, "processed", ".staging"))
		},
		"other empty directories": func(t *testing.T, root string) {
			t.Helper()
			mustMkdir(t, filepath.Join(root, "processed", "environment", "empty"))
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			setup(t, root)
			paths, err := Prepare(root, config.Default().Storage)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if _, err := os.Stat(paths.Staging); err != nil {
				t.Fatalf("Stat(staging) error = %v", err)
			}
			if _, err := os.Stat(paths.Database); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("database was created before SQLite bootstrap: %v", err)
			}
		})
	}
}

func TestPrepareRefusesMissingDatabaseWithProcessedData(t *testing.T) {
	tests := map[string]func(*testing.T, string) string{
		"managed file nested": func(t *testing.T, root string) string {
			t.Helper()
			path := filepath.Join(root, "processed", "vehicle", "asset.png")
			mustWrite(t, path, []byte("managed"))
			return path
		},
		"staging file": func(t *testing.T, root string) string {
			t.Helper()
			path := filepath.Join(root, "processed", ".staging", "pending.bin")
			mustWrite(t, path, []byte("staged"))
			return path
		},
		"staging directory entry": func(t *testing.T, root string) string {
			t.Helper()
			path := filepath.Join(root, "processed", ".staging", "pending")
			mustMkdir(t, path)
			return path
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			protectedPath := setup(t, root)
			before, _ := os.ReadFile(protectedPath)

			_, err := Prepare(root, config.Default().Storage)
			var recoveryErr *DatabaseRecoveryError
			if !errors.As(err, &recoveryErr) {
				t.Fatalf("Prepare() error = %v, want DatabaseRecoveryError", err)
			}
			if !bytes.Contains([]byte(err.Error()), []byte("restore assets.db or move")) {
				t.Fatalf("error = %q, want actionable recovery guidance", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, "assets.db")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("database exists after refusal: %v", statErr)
			}
			after, readErr := os.ReadFile(protectedPath)
			if readErr == nil && !bytes.Equal(before, after) {
				t.Fatal("processed data changed during refusal")
			}
			if readErr != nil {
				if info, statErr := os.Stat(protectedPath); statErr != nil || !info.IsDir() {
					t.Fatalf("protected staging entry changed: read=%v stat=%v", readErr, statErr)
				}
			}
		})
	}
}

func TestPreparePreservesExistingZeroByteDatabaseForSQLiteValidation(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "assets.db"), nil)
	mustWrite(t, filepath.Join(root, "processed", "asset.png"), []byte("managed"))

	if _, err := Prepare(root, config.Default().Storage); err != nil {
		t.Fatalf("Prepare() error = %v, want existing database left for SQLite validation", err)
	}
	info, statErr := os.Stat(filepath.Join(root, "assets.db"))
	if statErr != nil || info.Size() != 0 {
		t.Fatalf("zero-byte database changed: info=%v error=%v", info, statErr)
	}
}

func TestPrepareRefusesSymlinkOrSpecialProcessedEntry(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.bin")
	mustWrite(t, target, []byte("target"))
	link := filepath.Join(root, "processed", "linked.bin")
	mustMkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	_, err := Prepare(root, config.Default().Storage)
	var recoveryErr *DatabaseRecoveryError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("Prepare() error = %v, want DatabaseRecoveryError", err)
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Fatalf("processed symlink was altered: %v", statErr)
	}
}

func TestPrepareDoesNotApplyMissingDatabaseGuardToExistingDatabase(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "assets.db"), []byte("existing database bytes"))
	mustWrite(t, filepath.Join(root, "processed", "asset.png"), []byte("managed"))

	if _, err := Prepare(root, config.Default().Storage); err != nil {
		t.Fatalf("Prepare() error = %v, want existing database left for SQLite verification", err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
