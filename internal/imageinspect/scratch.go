package imageinspect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dfcut8/assets-library-manager/internal/importer"
)

// ScratchStore owns a root containing per-item analysis directories.
type ScratchStore struct {
	absoluteParent string
	root           *os.Root
}

// ScratchFile is the sole rendition file inside one item scratch directory.
type ScratchFile struct {
	Directory    string
	RelativePath string
	AbsolutePath string
}

// OpenScratchStore opens an existing canonical parent for transient rendition files.
func OpenScratchStore(parent string) (*ScratchStore, error) {
	absolute, err := filepath.Abs(parent)
	if err != nil {
		return nil, fmt.Errorf("resolving scratch parent: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing scratch parent: %w", err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, fmt.Errorf("opening scratch root: %w", err)
	}

	return &ScratchStore{absoluteParent: canonical, root: root}, nil
}

// Create writes and syncs the sole analysis rendition for an item.
func (store *ScratchStore) Create(
	ctx context.Context,
	itemID importer.ID,
	rendition Rendition,
) (scratch ScratchFile, returnErr error) {
	if err := ctx.Err(); err != nil {
		return ScratchFile{}, fmt.Errorf("creating analysis scratch: %w", err)
	}
	if itemID.IsZero() || len(rendition.Data) == 0 ||
		(rendition.Extension != ".png" && rendition.Extension != ".jpg") {
		return ScratchFile{}, errors.New("creating analysis scratch: rendition is invalid")
	}
	directory := itemID.String() + ".scratch"
	if err := store.root.Mkdir(directory, 0o700); err != nil {
		return ScratchFile{}, fmt.Errorf("creating item scratch directory: %w", err)
	}
	keepDirectory := false
	defer func() {
		if !keepDirectory {
			if err := store.root.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
				returnErr = errors.Join(returnErr, fmt.Errorf("removing incomplete scratch directory: %w", err))
			}
		}
	}()

	fileName := "analysis" + rendition.Extension
	relativePath := path.Join(directory, fileName)
	file, err := store.root.OpenFile(relativePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ScratchFile{}, fmt.Errorf("creating analysis scratch file: %w", err)
	}
	closed := false
	keepFile := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, closeScratchFile(file))
		}
		if !keepFile {
			if err := store.root.Remove(relativePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				returnErr = errors.Join(returnErr, fmt.Errorf("removing incomplete scratch file: %w", err))
			}
		}
	}()
	if err := writeScratchFile(file, rendition.Data); err != nil {
		return ScratchFile{}, fmt.Errorf("writing analysis scratch file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return ScratchFile{}, fmt.Errorf("syncing analysis scratch file: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return ScratchFile{}, fmt.Errorf("closing analysis scratch file: %w", err)
	}
	closed = true
	keepFile = true
	keepDirectory = true

	return ScratchFile{
		Directory: directory, RelativePath: relativePath,
		AbsolutePath: filepath.Join(store.absoluteParent, filepath.FromSlash(relativePath)),
	}, nil
}

// Remove deletes the exact rendition and then its now-empty item directory.
func (store *ScratchStore) Remove(ctx context.Context, scratch ScratchFile) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("removing analysis scratch: %w", err)
	}
	if !validScratchFile(scratch) {
		return errors.New("removing analysis scratch: path is invalid")
	}
	if err := store.root.Remove(scratch.RelativePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing analysis scratch file: %w", err)
	}
	if err := store.root.Remove(scratch.Directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing analysis scratch directory: %w", err)
	}
	if runtime.GOOS != "windows" {
		parent, err := store.root.Open(".")
		if err != nil {
			return fmt.Errorf("opening scratch root for sync: %w", err)
		}
		syncErr := parent.Sync()
		closeErr := closeScratchFile(parent)
		if syncErr != nil || closeErr != nil {
			return errors.Join(wrapScratchError("syncing scratch root", syncErr), closeErr)
		}
	}

	return nil
}

// Close releases the scratch root handle.
func (store *ScratchStore) Close() error {
	if err := store.root.Close(); err != nil {
		return fmt.Errorf("closing scratch root: %w", err)
	}

	return nil
}

func validScratchFile(scratch ScratchFile) bool {
	if !strings.HasSuffix(scratch.Directory, ".scratch") || strings.ContainsRune(scratch.Directory, '/') {
		return false
	}
	identifier := strings.TrimSuffix(scratch.Directory, ".scratch")
	if _, err := importer.ParseID(identifier); err != nil {
		return false
	}
	wantPNG := path.Join(scratch.Directory, "analysis.png")
	wantJPEG := path.Join(scratch.Directory, "analysis.jpg")

	return scratch.RelativePath == wantPNG || scratch.RelativePath == wantJPEG
}

func closeScratchFile(file *os.File) error {
	if file == nil {
		return nil
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing scratch file: %w", err)
	}

	return nil
}

func writeScratchFile(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}

	return nil
}

func wrapScratchError(operation string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%s: %w", operation, err)
}
