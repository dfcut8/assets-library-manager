package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type migration struct {
	version  int
	name     string
	checksum string
	sql      string
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func applyMigrations(ctx context.Context, db *sql.DB, files fs.FS) (returnErr error) {
	migrations, err := readMigrations(files)
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting migration transaction: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("rolling back migrations: %w", err))
		}
	}()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			checksum TEXT NOT NULL CHECK(length(checksum) = 64),
			applied_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		return fmt.Errorf("creating migration ledger: %w", err)
	}

	applied, err := readApplied(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateApplied(migrations, applied); err != nil {
		return err
	}
	for _, item := range migrations {
		if _, exists := applied[item.version]; exists {
			continue
		}

		if _, err := tx.ExecContext(ctx, item.sql); err != nil {
			return fmt.Errorf("applying migration %d: %w", item.version, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES(?, ?, ?, ?)",
			item.version,
			item.name,
			item.checksum,
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("recording migration %d: %w", item.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migrations: %w", err)
	}

	return nil
}

func verifyAppliedMigrations(ctx context.Context, queryer rowsQuerier, files fs.FS) error {
	migrations, err := readMigrations(files)
	if err != nil {
		return err
	}
	applied, err := readApplied(ctx, queryer)
	if err != nil {
		return err
	}

	return validateApplied(migrations, applied)
}

func validateApplied(migrations []migration, applied map[int]string) error {
	knownVersions := make(map[int]struct{}, len(migrations))
	for _, item := range migrations {
		knownVersions[item.version] = struct{}{}
		if checksum, exists := applied[item.version]; exists && checksum != item.checksum {
			return fmt.Errorf("verifying migration %d: checksum mismatch", item.version)
		}
	}
	for version := range applied {
		if _, exists := knownVersions[version]; !exists {
			return fmt.Errorf("verifying migrations: database contains unknown version %d", version)
		}
	}

	return nil
}

func readMigrations(files fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}

	items := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		parts := strings.SplitN(entry.Name(), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("reading embedded migrations: invalid name %q", entry.Name())
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil || version < 1 {
			return nil, fmt.Errorf("reading embedded migrations: invalid version in %q", entry.Name())
		}
		data, err := fs.ReadFile(files, "migrations/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("reading migration %d: %w", version, err)
		}
		digest := sha256.Sum256(data)
		items = append(items, migration{
			version:  version,
			name:     entry.Name(),
			checksum: hex.EncodeToString(digest[:]),
			sql:      string(data),
		})
	}
	sort.Slice(items, func(left, right int) bool {
		return items[left].version < items[right].version
	})
	for index := 1; index < len(items); index++ {
		if items[index-1].version == items[index].version {
			return nil, fmt.Errorf("reading embedded migrations: duplicate version %d", items[index].version)
		}
	}

	return items, nil
}

func readApplied(ctx context.Context, queryer rowsQuerier) (applied map[int]string, returnErr error) {
	rows, err := queryer.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing applied migrations: %w", err))
		}
	}()

	applied = map[int]string{}
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scanning applied migration: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating applied migrations: %w", err)
	}

	return applied, nil
}
