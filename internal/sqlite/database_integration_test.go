//go:build integration

package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/importer"
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

func TestOpenMigratesImportSourceIdentityWithoutDataLoss(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "assets.db")
	initialMigration, err := migrationFiles.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	v1Files := fstest.MapFS{
		"migrations/001_initial.sql": {Data: initialMigration},
	}
	v1Database, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := v1Database.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, v1Database, v1Files); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	sourceID := strings.Repeat("a", 32)
	itemID := strings.Repeat("b", 32)
	runID := strings.Repeat("c", 32)
	fingerprint := strings.Repeat("1", 64)
	if _, err := v1Database.ExecContext(ctx, `
		INSERT INTO import_sources(
			id, source_path, source_type, discovery_fingerprint, state,
			deletion_state, discovered_at, updated_at
		) VALUES(?, 'reused.png', 'loose', ?, 'deleted', 'deleted', ?, ?)
	`, sourceID, fingerprint, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := v1Database.ExecContext(ctx, `
		INSERT INTO import_items(
			id, source_id, state, attempt_count, created_at, updated_at
		) VALUES(?, ?, 'failed', 2, ?, ?)
	`, itemID, sourceID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := v1Database.ExecContext(ctx, `
		INSERT INTO ai_runs(
			id, import_item_id, provider, model, reasoning_effort, image_detail,
			prompt_version, schema_version, attempt_number, started_at, outcome
		) VALUES(?, ?, 'openai', 'test-model', 'medium', 'auto', 'v1', 'v1', 2, ?, 'permanent-error')
	`, runID, itemID, now); err != nil {
		t.Fatal(err)
	}
	if err := v1Database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(v1) error = %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	assertDatabaseState(t, database)

	var preserved int
	if err := database.db.QueryRowContext(ctx, `
		SELECT count(*)
		FROM import_sources AS source
		JOIN import_items AS item ON item.source_id = source.id
		JOIN ai_runs AS run ON run.import_item_id = item.id
		WHERE source.id = ? AND item.id = ? AND run.id = ? AND item.attempt_count = 2
	`, sourceID, itemID, runID).Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if preserved != 1 {
		t.Fatalf("preserved workflow count = %d, want 1", preserved)
	}

	rows, err := database.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, rowID, parent, foreignKey any
		if err := rows.Scan(&table, &rowID, &parent, &foreignKey); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("foreign key violation: table=%v row=%v parent=%v key=%v", table, rowID, parent, foreignKey)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	sourcePath, err := importer.NewSourcePath("reused.png")
	if err != nil {
		t.Fatal(err)
	}
	replacementNow := time.Now().UTC()
	replacement := importer.SourceRecord{
		ID: integrationID(t, strings.Repeat("d", 32)), Path: sourcePath,
		Type:                 importer.SourceTypeLoose,
		DiscoveryFingerprint: importer.NewDigest(sha256.Sum256([]byte("replacement"))),
		State:                importer.SourceStateDiscovered, DeletionState: importer.DeletionStateNotEligible,
		DiscoveredAt: replacementNow, UpdatedAt: replacementNow,
	}
	if created, err := database.CreateImportSource(ctx, replacement); err != nil || created != replacement {
		t.Fatalf("CreateImportSource(reused path) = %+v, %v", created, err)
	}

	if _, err := database.db.ExecContext(ctx, "DELETE FROM import_sources WHERE id = ?", sourceID); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name  string
		query string
		id    string
	}{
		{name: "import_items", query: "SELECT count(*) FROM import_items WHERE id = ?", id: itemID},
		{name: "ai_runs", query: "SELECT count(*) FROM ai_runs WHERE id = ?", id: runID},
	} {
		var count int
		if err := database.db.QueryRowContext(ctx, check.query, check.id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("cascaded %s count = %d, want 0", check.name, count)
		}
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
	if migrations != 2 {
		t.Fatalf("migration count = %d, want 2", migrations)
	}
}
