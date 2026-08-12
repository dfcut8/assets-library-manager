// Package storage validates and prepares executable-relative runtime paths.
package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dfcut8/assets-library-manager/internal/config"
)

// DatabaseRecoveryError reports that recreating a missing database would orphan managed data.
type DatabaseRecoveryError struct{}

// Error describes the safe recovery actions without exposing an absolute filesystem path.
func (*DatabaseRecoveryError) Error() string {
	return "storage: database is missing while processed data exists; catalog metadata may have been lost; restore assets.db or move the processed data to a safe location before restarting"
}

// Paths contains canonical runtime locations beneath the application root.
type Paths struct {
	Root      string
	Database  string
	Incoming  string
	Processed string
	Staging   string
}

// Prepare validates recovery preconditions before creating missing directories.
func Prepare(root string, cfg config.StorageConfig) (Paths, error) {
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return Paths{}, fmt.Errorf("resolving application root: %w", err)
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return Paths{}, fmt.Errorf("canonicalizing application root: %w", err)
	}

	paths := Paths{
		Root:      rootPath,
		Database:  filepath.Join(rootPath, cfg.Database),
		Incoming:  filepath.Join(rootPath, cfg.IncomingDirectory),
		Processed: filepath.Join(rootPath, cfg.ProcessedDirectory),
		Staging:   filepath.Join(rootPath, cfg.ProcessedDirectory, ".staging"),
	}

	isMissing, err := databaseMissing(paths.Database)
	if err != nil {
		return Paths{}, err
	}
	if isMissing {
		hasData, inspectErr := processedHasData(paths.Processed)
		if inspectErr != nil {
			return Paths{}, fmt.Errorf("inspecting processed data: %w", inspectErr)
		}
		if hasData {
			return Paths{}, &DatabaseRecoveryError{}
		}
	}

	for _, dir := range []string{
		filepath.Dir(paths.Database),
		paths.Incoming,
		paths.Processed,
		paths.Staging,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Paths{}, fmt.Errorf("creating runtime directory: %w", err)
		}
	}

	for _, path := range []string{
		filepath.Dir(paths.Database),
		paths.Incoming,
		paths.Processed,
		paths.Staging,
	} {
		if err := verifyContainedDirectory(rootPath, path); err != nil {
			return Paths{}, err
		}
	}
	if err := verifyDistinct(paths); err != nil {
		return Paths{}, err
	}
	if info, statErr := os.Lstat(paths.Database); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return Paths{}, errors.New("storage: database must be a regular file")
		}
		if err := verifyContainedFile(rootPath, paths.Database); err != nil {
			return Paths{}, err
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return Paths{}, fmt.Errorf("stating database: %w", statErr)
	}

	return paths, nil
}

func databaseMissing(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("stating database: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("storage: database must be a regular file")
	}

	return false, nil
}

func processedHasData(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return true, nil
	}

	hasData := false
	err = filepath.WalkDir(path, func(entryPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entryPath == path {
			return nil
		}

		rel, relErr := filepath.Rel(path, entryPath)
		if relErr != nil {
			return relErr
		}
		if rel == ".staging" && entry.IsDir() {
			return nil
		}
		isStagedEntry := strings.HasPrefix(rel, ".staging"+string(filepath.Separator))
		if isStagedEntry || !entry.IsDir() {
			hasData = true
			return fs.SkipAll
		}

		return nil
	})

	return hasData, err
}

func verifyContainedDirectory(root, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stating runtime directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("storage: runtime directories must not be symlinks or special files")
	}

	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("canonicalizing runtime directory: %w", err)
	}
	if !isWithin(root, realPath) {
		return errors.New("storage: runtime directory escapes the application root")
	}

	return nil
}

func verifyContainedFile(root, path string) error {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("canonicalizing database: %w", err)
	}
	if !isWithin(root, realPath) {
		return errors.New("storage: database escapes the application root")
	}

	return nil
}

func verifyDistinct(paths Paths) error {
	realIncoming, err := filepath.EvalSymlinks(paths.Incoming)
	if err != nil {
		return fmt.Errorf("canonicalizing incoming directory: %w", err)
	}
	realProcessed, err := filepath.EvalSymlinks(paths.Processed)
	if err != nil {
		return fmt.Errorf("canonicalizing processed directory: %w", err)
	}
	realDatabaseParent, err := filepath.EvalSymlinks(filepath.Dir(paths.Database))
	if err != nil {
		return fmt.Errorf("canonicalizing database directory: %w", err)
	}
	realDatabase := filepath.Join(realDatabaseParent, filepath.Base(paths.Database))

	if overlaps(realIncoming, realProcessed) || overlaps(realIncoming, realDatabase) ||
		overlaps(realProcessed, realDatabase) {
		return errors.New("storage: canonical runtime paths must not overlap")
	}

	return nil
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func overlaps(left, right string) bool {
	return isWithin(left, right) || isWithin(right, left)
}
