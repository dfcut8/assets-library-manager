// Package sqlite owns the embedded SQLite database, migrations, and startup verification.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Register the CGO-free database/sql driver.
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Database wraps the process-owned database connection pool.
type Database struct {
	db *sql.DB
}

// Open creates or opens, migrates, and verifies a database before returning it.
func Open(ctx context.Context, path string) (*Database, error) {
	exists := false
	info, statErr := os.Lstat(path)
	if statErr == nil {
		exists = true
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("opening sqlite: existing database is not a regular file")
		}
		if info.Size() == 0 {
			return nil, errors.New("opening sqlite: existing database is empty and will not be replaced")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stating sqlite database: %w", statErr)
	}
	if exists {
		if err := preflightExisting(ctx, path); err != nil {
			return nil, err
		}
	}

	dsn := dataSourceName(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	closeOnError := func(cause error) (*Database, error) {
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(cause, fmt.Errorf("closing sqlite: %w", closeErr))
		}
		return nil, cause
	}

	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("connecting to sqlite: %w", err))
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return closeOnError(fmt.Errorf("restricting database permissions: %w", err))
	}
	if err := applyMigrations(ctx, db, migrationFiles); err != nil {
		return closeOnError(err)
	}
	if err := verify(ctx, db); err != nil {
		return closeOnError(err)
	}

	return &Database{db: db}, nil
}

func preflightExisting(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", readOnlyDataSourceName(path))
	if err != nil {
		return fmt.Errorf("opening existing sqlite read-only: %w", err)
	}
	closeWithError := func(cause error) error {
		if closeErr := db.Close(); closeErr != nil {
			return errors.Join(cause, fmt.Errorf("closing existing sqlite preflight: %w", closeErr))
		}

		return cause
	}
	if err := db.PingContext(ctx); err != nil {
		return closeWithError(fmt.Errorf("reading existing sqlite: %w", err))
	}

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return closeWithError(fmt.Errorf("checking existing sqlite integrity: %w", err))
	}
	if integrity != "ok" {
		return closeWithError(fmt.Errorf("checking existing sqlite integrity: %s", integrity))
	}

	var ledgerCount int
	if err := db.QueryRowContext(
		ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'",
	).Scan(&ledgerCount); err != nil {
		return closeWithError(fmt.Errorf("checking existing sqlite migration ledger: %w", err))
	}
	if ledgerCount != 1 {
		return closeWithError(errors.New("checking existing sqlite migration ledger: database is incompatible and will not be replaced"))
	}
	if err := verifyAppliedMigrations(ctx, db, migrationFiles); err != nil {
		return closeWithError(err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("closing existing sqlite preflight: %w", err)
	}

	return nil
}

// Checkpoint attempts to merge the write-ahead log into the main database before shutdown.
func (d *Database) Checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointedFrames int
	if err := d.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(
		&busy,
		&logFrames,
		&checkpointedFrames,
	); err != nil {
		return fmt.Errorf("checkpointing sqlite: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpointing sqlite: database remained busy with %d log frames", logFrames)
	}

	return nil
}

// Close releases the connection pool owned by the database.
func (d *Database) Close() error {
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("closing sqlite: %w", err)
	}

	return nil
}

func dataSourceName(path string) string {
	return sourceName(path, false)
}

func readOnlyDataSourceName(path string) string {
	return sourceName(path, true)
}

func sourceName(path string, readOnly bool) string {
	slashPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		slashPath = "/" + slashPath
	}
	dsn := &url.URL{
		Scheme: "file",
		Path:   slashPath,
	}
	query := dsn.Query()
	if readOnly {
		query.Set("mode", "ro")
		query.Set("_query_only", "true")
		dsn.RawQuery = query.Encode()

		return dsn.String()
	}
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "FULL")
	query.Set("_dqs", "false")
	query.Set("_txlock", "immediate")
	dsn.RawQuery = query.Encode()

	return dsn.String()
}

func verify(ctx context.Context, db *sql.DB) error {
	expectedPragmas := []struct {
		query    string
		expected string
	}{
		{query: "PRAGMA journal_mode", expected: "wal"},
		{query: "PRAGMA synchronous", expected: "2"},
		{query: "PRAGMA foreign_keys", expected: "1"},
	}
	for _, check := range expectedPragmas {
		var actual string
		if err := db.QueryRowContext(ctx, check.query).Scan(&actual); err != nil {
			return fmt.Errorf("verifying sqlite pragma: %w", err)
		}
		if !strings.EqualFold(actual, check.expected) {
			return fmt.Errorf("verifying sqlite pragma: got %q, expected %q", actual, check.expected)
		}
	}

	if _, err := db.ExecContext(ctx, "CREATE VIRTUAL TABLE temp.fts5_probe USING fts5(value)"); err != nil {
		return fmt.Errorf("verifying sqlite fts5: %w", err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE temp.fts5_probe"); err != nil {
		return fmt.Errorf("dropping sqlite fts5 probe: %w", err)
	}

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return fmt.Errorf("checking sqlite integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("checking sqlite integrity: %s", integrity)
	}

	return nil
}
