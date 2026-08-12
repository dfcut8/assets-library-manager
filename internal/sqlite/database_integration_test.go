//go:build integration

package sqlite

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesMigratesVerifiesAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "assets.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	database, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(new) error = %v", err)
	}
	assertDatabaseState(t, database)
	assertStaticSpriteSheetAndSearch(t, database)
	if err := database.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint() error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	database, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(existing) error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertStaticSpriteSheetAndSearch(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	assetID := strings.Repeat("a", 32)
	digest := bytes.Repeat([]byte{1}, 32)
	_, err := database.db.ExecContext(ctx, `
		INSERT INTO assets(
			id, sha256, original_filename, managed_path, format, mime_type,
			file_size_bytes, display_width, display_height, orientation_class,
			has_alpha, has_transparency, encoded_animated, encoded_frame_count,
			title, primary_type, layout_kind, state, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'png', 'image/png', 2766, 64, 64, 'square', 1, 1, 0, 1, ?, 'vehicle', 'sprite-sheet', 'ready', ?, ?)
	`, assetID, digest, "Battleship_03_Idle.png", "vehicle/battleship.png", "Pink Battleship", now, now)
	if err != nil {
		t.Fatalf("inserting static sprite sheet: %v", err)
	}
	_, err = database.db.ExecContext(ctx, `
		INSERT INTO asset_sheet_layouts(
			asset_id, kind, columns_count, rows_count, cell_width, cell_height,
			frame_count, animation_label, updated_at
		) VALUES (?, 'sprite-sheet', 4, 4, 16, 16, 16, 'idle', ?)
	`, assetID, now)
	if err != nil {
		t.Fatalf("inserting sprite layout: %v", err)
	}

	var matches int
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM asset_search WHERE asset_search MATCH 'battleship'").Scan(&matches); err != nil {
		t.Fatalf("querying FTS5: %v", err)
	}
	if matches != 1 {
		t.Fatalf("FTS5 matches = %d, want 1", matches)
	}
}

func TestOpenPreservesCorruptExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assets.db")
	want := []byte("not a sqlite database")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("Open(corrupt) error = nil")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("corrupt database was overwritten")
	}
}

func TestOpenPreservesEmptyExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assets.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "will not be replaced") {
		t.Fatalf("Open(empty) error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("empty database changed: info=%v error=%v", info, err)
	}
}

func TestOpenRejectsMigrationChecksumConflict(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "assets.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "UPDATE schema_migrations SET checksum = ? WHERE version = 1", strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Open(checksum conflict) error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("checksum-conflicted database was modified during refusal")
	}
}

func assertDatabaseState(t *testing.T, database *Database) {
	t.Helper()
	for _, name := range []string{"assets", "asset_sheet_layouts", "thumbnails", "tags", "asset_tags", "import_sources", "import_items", "ai_runs", "asset_search"} {
		var count int
		if err := database.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = ?", name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("schema object %q count = %d, want 1", name, count)
		}
	}
	var migrations int
	if err := database.db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 1 {
		t.Fatalf("migration count = %d, want 1", migrations)
	}
}
