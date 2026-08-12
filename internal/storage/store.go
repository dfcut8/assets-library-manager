package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"slices"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/importer"
)

const copyBufferSize = 64 << 10

// Store owns root-scoped access to routine incoming and processed file operations.
type Store struct {
	incoming  *os.Root
	processed *os.Root
	staging   *os.Root
}

// StagedFile is a fully written and synced original awaiting promotion.
type StagedFile struct {
	Path   importer.StagedPath
	Digest importer.Digest
	Size   int64
}

// SnapshotIncoming returns top-level entry metadata without following symbolic links.
func (s *Store) SnapshotIncoming(ctx context.Context) ([]importer.IncomingEntry, error) {
	entries, err := fs.ReadDir(s.incoming.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("reading incoming root: %w", err)
	}
	snapshot := make([]importer.IncomingEntry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("snapshotting incoming root: %w", err)
		}
		info, err := s.incoming.Lstat(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("stating incoming entry: %w", err)
		}
		snapshot = append(snapshot, importer.IncomingEntry{
			Name: entry.Name(), Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime(),
		})
	}

	return snapshot, nil
}

// OpenStore opens the prepared runtime roots. The caller owns the returned Store.
func OpenStore(paths Paths) (*Store, error) {
	incoming, err := os.OpenRoot(paths.Incoming)
	if err != nil {
		return nil, fmt.Errorf("opening incoming root: %w", err)
	}
	processed, err := os.OpenRoot(paths.Processed)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("opening processed root: %w", err),
			closeRoot("incoming", incoming),
		)
	}
	staging, err := os.OpenRoot(paths.Staging)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("opening staging root: %w", err),
			closeRoot("processed", processed),
			closeRoot("incoming", incoming),
		)
	}

	return &Store{incoming: incoming, processed: processed, staging: staging}, nil
}

// Stage streams an original into its exclusive staging file while enforcing a byte limit.
func (s *Store) Stage(
	ctx context.Context,
	itemID importer.ID,
	source io.Reader,
	maxBytes int64,
) (staged StagedFile, returnErr error) {
	if source == nil {
		return StagedFile{}, errors.New("staging asset: source reader is nil")
	}
	if maxBytes < 1 {
		return StagedFile{}, errors.New("staging asset: byte limit must be positive")
	}
	stagedPath, err := importer.NewStagedPath(itemID)
	if err != nil {
		return StagedFile{}, err
	}
	file, err := s.staging.OpenFile(stagedPath.String(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return StagedFile{}, fmt.Errorf("creating staging file: %w", err)
	}
	closed := false
	keep := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, closeFile("staging file", file))
		}
		if !keep {
			if err := s.staging.Remove(stagedPath.String()); err != nil && !errors.Is(err, fs.ErrNotExist) {
				returnErr = errors.Join(returnErr, fmt.Errorf("removing incomplete staging file: %w", err))
			}
		}
	}()

	hasher := sha256.New()
	buffer := make([]byte, copyBufferSize)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return StagedFile{}, fmt.Errorf("staging asset: %w", err)
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			if int64(read) > maxBytes-size {
				return StagedFile{}, fmt.Errorf("staging asset: source exceeds %d bytes", maxBytes)
			}
			if err := writeFull(file, buffer[:read]); err != nil {
				return StagedFile{}, fmt.Errorf("writing staging file: %w", err)
			}
			if _, err := hasher.Write(buffer[:read]); err != nil {
				return StagedFile{}, fmt.Errorf("hashing staging file: %w", err)
			}
			size += int64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return StagedFile{}, fmt.Errorf("reading asset source: %w", readErr)
		}
		if read == 0 {
			return StagedFile{}, io.ErrNoProgress
		}
	}
	if err := file.Sync(); err != nil {
		return StagedFile{}, fmt.Errorf("syncing staging file: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return StagedFile{}, fmt.Errorf("closing staging file: %w", err)
	}
	closed = true
	keep = true

	var digest importer.Digest
	copy(digest[:], hasher.Sum(nil))

	return StagedFile{Path: stagedPath, Digest: digest, Size: size}, nil
}

// FingerprintIncoming hashes one validated regular incoming source.
func (s *Store) FingerprintIncoming(
	ctx context.Context,
	sourcePath importer.SourcePath,
	maxBytes int64,
) (importer.Digest, int64, error) {
	file, err := s.openRegular(s.incoming, sourcePath.String(), "incoming source")
	if err != nil {
		return importer.Digest{}, 0, err
	}
	digest, size, hashErr := hashReader(ctx, file, maxBytes)
	closeErr := closeFile("incoming source", file)
	if hashErr != nil || closeErr != nil {
		return importer.Digest{}, 0, errors.Join(hashErr, closeErr)
	}

	return digest, size, nil
}

// OpenIncoming opens one validated regular source for streaming or ZIP random access.
func (s *Store) OpenIncoming(sourcePath importer.SourcePath) (*os.File, error) {
	return s.openRegular(s.incoming, sourcePath.String(), "incoming source")
}

// OpenStaged opens a validated regular staged file.
func (s *Store) OpenStaged(stagedPath importer.StagedPath) (*os.File, error) {
	return s.openRegular(s.staging, stagedPath.String(), "staging file")
}

// RemoveStaged removes an item staging file idempotently.
func (s *Store) RemoveStaged(stagedPath importer.StagedPath) error {
	if err := s.staging.Remove(stagedPath.String()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing staging file: %w", err)
	}

	return nil
}

// Promote publishes a staged original without overwriting an unrelated destination.
func (s *Store) Promote(
	ctx context.Context,
	staged StagedFile,
	destination importer.ManagedPath,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("promoting staged asset: %w", err)
	}
	parent := path.Dir(destination.String())
	if err := s.processed.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("creating managed directory: %w", err)
	}
	stagedUnderProcessed := path.Join(".staging", staged.Path.String())
	if err := s.processed.Link(stagedUnderProcessed, destination.String()); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("promoting staged asset: %w", err)
		}
		matches, verifyErr := s.VerifyManaged(ctx, destination, staged.Digest, staged.Size)
		if verifyErr != nil {
			return verifyErr
		}
		if !matches {
			return fmt.Errorf("promoting staged asset: %w", importer.ErrConflict)
		}
		return s.RemoveStaged(staged.Path)
	}
	if err := syncRootFile(s.processed, destination.String()); err != nil {
		return err
	}
	if err := syncRootDirectory(s.processed, parent); err != nil {
		return err
	}
	if err := s.RemoveStaged(staged.Path); err != nil {
		return err
	}
	if err := syncRootDirectory(s.processed, ".staging"); err != nil {
		return err
	}

	return nil
}

// VerifyManaged checks a regular managed file against its full expected digest and size.
func (s *Store) VerifyManaged(
	ctx context.Context,
	managedPath importer.ManagedPath,
	expected importer.Digest,
	maxBytes int64,
) (bool, error) {
	file, err := s.openRegular(s.processed, managedPath.String(), "managed asset")
	if err != nil {
		return false, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return false, errors.Join(fmt.Errorf("stating managed asset: %w", statErr), closeFile("managed asset", file))
	}
	if info.Size() != maxBytes {
		return false, closeFile("managed asset", file)
	}
	digest, size, hashErr := hashReader(ctx, file, maxBytes)
	closeErr := closeFile("managed asset", file)
	if hashErr != nil || closeErr != nil {
		return false, errors.Join(hashErr, closeErr)
	}

	return size == maxBytes && digest == expected, nil
}

// OpenManaged opens one validated regular managed original for streaming.
func (s *Store) OpenManaged(managedPath importer.ManagedPath) (*os.File, error) {
	return s.openRegular(s.processed, managedPath.String(), "managed asset")
}

// DeleteIncoming revalidates the complete source digest immediately before deletion.
func (s *Store) DeleteIncoming(
	ctx context.Context,
	sourcePath importer.SourcePath,
	expected importer.Digest,
	maxBytes int64,
) error {
	digest, _, err := s.FingerprintIncoming(ctx, sourcePath, maxBytes)
	if err != nil {
		return err
	}
	if digest != expected {
		return importer.ErrSourceChanged
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("deleting incoming source: %w", err)
	}
	if err := s.incoming.Remove(sourcePath.String()); err != nil {
		return fmt.Errorf("deleting incoming source: %w", err)
	}

	return nil
}

// CleanOrphanStaging removes only old, regular, correctly named, unreferenced staging files.
func (s *Store) CleanOrphanStaging(
	ctx context.Context,
	referenced []importer.StagedPath,
	olderThan time.Time,
) ([]importer.StagedPath, error) {
	entries, err := fs.ReadDir(s.staging.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("reading staging root: %w", err)
	}
	referencedNames := make([]string, 0, len(referenced))
	for _, stagedPath := range referenced {
		referencedNames = append(referencedNames, stagedPath.String())
	}
	slices.Sort(referencedNames)

	removed := make([]importer.StagedPath, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("cleaning orphan staging files: %w", err)
		}
		stagedPath, parseErr := importer.ParseStagedPath(entry.Name())
		_, isReferenced := slices.BinarySearch(referencedNames, entry.Name())
		if parseErr != nil || isReferenced {
			continue
		}
		info, err := s.staging.Lstat(entry.Name())
		if err != nil {
			return removed, fmt.Errorf("stating staging entry: %w", err)
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(olderThan) {
			continue
		}
		if err := s.staging.Remove(entry.Name()); err != nil {
			return removed, fmt.Errorf("removing orphan staging file: %w", err)
		}
		removed = append(removed, stagedPath)
	}

	return removed, nil
}

// Close releases every root handle owned by the Store.
func (s *Store) Close() error {
	return errors.Join(
		closeRoot("staging", s.staging),
		closeRoot("processed", s.processed),
		closeRoot("incoming", s.incoming),
	)
}

func (s *Store) openRegular(root *os.Root, name, kind string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("stating %s: %w", kind, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("opening %s: path is not a regular file", kind)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", kind, err)
	}

	return file, nil
}

func hashReader(ctx context.Context, reader io.Reader, maxBytes int64) (importer.Digest, int64, error) {
	if maxBytes < 1 {
		return importer.Digest{}, 0, errors.New("hashing file: byte limit must be positive")
	}
	hasher := sha256.New()
	buffer := make([]byte, copyBufferSize)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return importer.Digest{}, 0, fmt.Errorf("hashing file: %w", err)
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			if int64(read) > maxBytes-size {
				return importer.Digest{}, 0, fmt.Errorf("hashing file: source exceeds %d bytes", maxBytes)
			}
			if _, err := hasher.Write(buffer[:read]); err != nil {
				return importer.Digest{}, 0, fmt.Errorf("hashing file: %w", err)
			}
			size += int64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return importer.Digest{}, 0, fmt.Errorf("hashing file: %w", readErr)
		}
		if read == 0 {
			return importer.Digest{}, 0, io.ErrNoProgress
		}
	}
	var digest importer.Digest
	copy(digest[:], hasher.Sum(nil))

	return digest, size, nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
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

func syncRootFile(root *os.Root, name string) error {
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening promoted asset for sync: %w", err)
	}
	syncErr := file.Sync()
	closeErr := closeFile("promoted asset", file)
	if syncErr != nil || closeErr != nil {
		return errors.Join(
			wrapError("syncing promoted asset", syncErr),
			closeErr,
		)
	}

	return nil
}

func syncRootDirectory(root *os.Root, name string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("opening managed directory for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := closeFile("managed directory", directory)
	if syncErr != nil || closeErr != nil {
		return errors.Join(
			wrapError("syncing managed directory", syncErr),
			closeErr,
		)
	}

	return nil
}

func closeRoot(name string, root *os.Root) error {
	if root == nil {
		return nil
	}
	if err := root.Close(); err != nil {
		return fmt.Errorf("closing %s root: %w", name, err)
	}

	return nil
}

func closeFile(name string, file *os.File) error {
	if file == nil {
		return nil
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", name, err)
	}

	return nil
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%s: %w", operation, err)
}
