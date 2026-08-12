package importer

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"path/filepath"
	"strings"
)

// Archive safety failures are stable expected conditions for source retention.
var (
	ErrUnsafeArchive    = errors.New("importer: unsafe archive")
	ErrArchiveLimit     = errors.New("importer: archive limit exceeded")
	ErrTruncatedArchive = errors.New("importer: truncated archive entry")
)

// ArchiveLimits bounds ZIP metadata and authoritative streamed content.
type ArchiveLimits struct {
	MaxEntries                int
	MaxEntryBytes             int64
	MaxTotalUncompressedBytes int64
	MaxCompressionRatio       float64
}

// ZIPEntryKind describes how one safe regular entry is handled.
type ZIPEntryKind string

// ZIP entry classifications recorded for source aggregation.
const (
	ZIPEntryImage     ZIPEntryKind = "image"
	ZIPEntryIgnored   ZIPEntryKind = "platform-metadata"
	ZIPEntryUnhandled ZIPEntryKind = "unhandled"
)

// ZIPEntry identifies one validated archive member.
type ZIPEntry struct {
	Name             string
	Extension        string
	Kind             ZIPEntryKind
	DeclaredSize     int64
	CompressedSize   int64
	ArchiveFileIndex int
}

// ZIPReport records deterministic classifications without retaining member bytes.
type ZIPReport struct {
	Entries           []ZIPEntry
	DirectoryCount    int
	ImageCount        int
	IgnoredCount      int
	UnhandledCount    int
	ActualStreamBytes int64
}

// ZIPImageVisitor consumes a complete supported member before ScanZIP advances.
type ZIPImageVisitor func(context.Context, ZIPEntry, io.Reader) error

// ScanZIP validates an archive and streams every regular entry under authoritative limits.
func ScanZIP(
	ctx context.Context,
	readerAt io.ReaderAt,
	size int64,
	limits ArchiveLimits,
	visit ZIPImageVisitor,
) (report ZIPReport, returnErr error) {
	if readerAt == nil || size < 1 {
		return ZIPReport{}, errors.New("scanning zip: reader and positive size are required")
	}
	if err := validateArchiveLimits(limits); err != nil {
		return ZIPReport{}, err
	}
	archive, err := zip.NewReader(readerAt, size)
	if err != nil {
		return ZIPReport{}, fmt.Errorf("opening zip: %w", err)
	}
	if len(archive.File) > limits.MaxEntries {
		return ZIPReport{}, fmt.Errorf("scanning zip: entry count: %w", ErrArchiveLimit)
	}

	report = ZIPReport{Entries: make([]ZIPEntry, 0, len(archive.File))}
	var declaredTotal int64
	for index, file := range archive.File {
		if err := ctx.Err(); err != nil {
			return ZIPReport{}, fmt.Errorf("scanning zip metadata: %w", err)
		}
		isDirectory := file.FileInfo().IsDir()
		if err := validateZIPEntry(file, isDirectory); err != nil {
			return ZIPReport{}, err
		}
		if isDirectory {
			report.DirectoryCount++
			continue
		}
		declaredSize, compressedSize, err := validateZIPSizes(file, limits)
		if err != nil {
			return ZIPReport{}, err
		}
		if declaredSize > limits.MaxTotalUncompressedBytes-declaredTotal {
			return ZIPReport{}, fmt.Errorf("scanning zip: declared total size: %w", ErrArchiveLimit)
		}
		declaredTotal += declaredSize

		extension := strings.ToLower(path.Ext(file.Name))
		if extension == ".zip" {
			return ZIPReport{}, fmt.Errorf("scanning zip entry %q: nested archive extension: %w", file.Name, ErrUnsafeArchive)
		}
		entry := ZIPEntry{
			Name: file.Name, Extension: extension, Kind: classifyZIPEntry(file.Name, extension),
			DeclaredSize: declaredSize, CompressedSize: compressedSize, ArchiveFileIndex: index,
		}
		switch entry.Kind {
		case ZIPEntryImage:
			report.ImageCount++
		case ZIPEntryIgnored:
			report.IgnoredCount++
		case ZIPEntryUnhandled:
			report.UnhandledCount++
		}
		report.Entries = append(report.Entries, entry)
	}

	var actualTotal int64
	for _, entry := range report.Entries {
		file := archive.File[entry.ArchiveFileIndex]
		streamErr := streamZIPEntry(ctx, file, entry, limits, &actualTotal, visit)
		report.ActualStreamBytes = actualTotal
		if streamErr != nil {
			return report, streamErr
		}
	}

	return report, nil
}

func streamZIPEntry(
	ctx context.Context,
	file *zip.File,
	entry ZIPEntry,
	limits ArchiveLimits,
	actualTotal *int64,
	visit ZIPImageVisitor,
) (returnErr error) {
	member, err := file.Open()
	if err != nil {
		return fmt.Errorf("opening zip entry %q: %w", entry.Name, err)
	}
	defer func() {
		if err := member.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing zip entry %q: %w", entry.Name, err))
		}
	}()

	limited := &archiveEntryReader{
		ctx: ctx, reader: member, entryLimit: limits.MaxEntryBytes,
		total: actualTotal, totalLimit: limits.MaxTotalUncompressedBytes,
	}
	buffered := bufio.NewReaderSize(limited, 16)
	header, peekErr := buffered.Peek(8)
	if peekErr != nil && !errors.Is(peekErr, io.EOF) && !errors.Is(peekErr, bufio.ErrBufferFull) {
		return fmt.Errorf("reading zip entry %q header: %w", entry.Name, classifyArchiveReadError(peekErr))
	}
	if hasZIPMagic(header) {
		return fmt.Errorf("scanning zip entry %q: nested archive content: %w", entry.Name, ErrUnsafeArchive)
	}

	var visitorErr error
	if entry.Kind == ZIPEntryImage {
		if visit == nil {
			visitorErr = errors.New("scanning zip: image visitor is nil")
		} else {
			visitorErr = visit(ctx, entry, buffered)
			if visitorErr != nil {
				visitorErr = fmt.Errorf("visiting zip image %q: %w", entry.Name, visitorErr)
			}
		}
	}
	_, drainErr := io.Copy(io.Discard, buffered)
	if drainErr != nil {
		drainErr = classifyArchiveReadError(drainErr)
		drainErr = fmt.Errorf("streaming zip entry %q: %w", entry.Name, drainErr)
	}
	if visitorErr != nil || drainErr != nil {
		return errors.Join(visitorErr, drainErr)
	}
	if limited.entryBytes != entry.DeclaredSize {
		return fmt.Errorf(
			"streaming zip entry %q: got %d bytes, declared %d: %w",
			entry.Name, limited.entryBytes, entry.DeclaredSize, ErrTruncatedArchive,
		)
	}
	if exceedsCompressionRatio(limited.entryBytes, entry.CompressedSize, limits.MaxCompressionRatio) {
		return fmt.Errorf("streaming zip entry %q: actual compression ratio: %w", entry.Name, ErrArchiveLimit)
	}

	return nil
}

func classifyArchiveReadError(err error) error {
	if errors.Is(err, ErrArchiveLimit) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	return errors.Join(ErrTruncatedArchive, err)
}

type archiveEntryReader struct {
	ctx        context.Context
	reader     io.Reader
	entryBytes int64
	entryLimit int64
	total      *int64
	totalLimit int64
}

func (reader *archiveEntryReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	entryRemaining := reader.entryLimit - reader.entryBytes
	totalRemaining := reader.totalLimit - *reader.total
	remaining := min(entryRemaining, totalRemaining)
	if remaining < 0 {
		return 0, ErrArchiveLimit
	}
	readBuffer := buffer
	if remaining < int64(len(readBuffer)) {
		readBuffer = readBuffer[:remaining+1]
	}
	count, err := reader.reader.Read(readBuffer)
	if int64(count) > remaining {
		return 0, ErrArchiveLimit
	}
	reader.entryBytes += int64(count)
	*reader.total += int64(count)
	if err != nil && !errors.Is(err, io.EOF) {
		err = classifyArchiveReadError(err)
	}

	return count, err
}

func validateArchiveLimits(limits ArchiveLimits) error {
	if limits.MaxEntries < 1 || limits.MaxEntryBytes < 1 ||
		limits.MaxTotalUncompressedBytes < 1 || limits.MaxCompressionRatio < 1 ||
		math.IsNaN(limits.MaxCompressionRatio) || math.IsInf(limits.MaxCompressionRatio, 0) {
		return errors.New("scanning zip: archive limits are invalid")
	}

	return nil
}

func validateZIPEntry(file *zip.File, isDirectory bool) error {
	name := file.Name
	if name == "" || strings.ContainsRune(name, '\x00') || strings.ContainsRune(name, '\\') {
		return fmt.Errorf("scanning zip entry: invalid path: %w", ErrUnsafeArchive)
	}
	normalized := name
	if isDirectory {
		normalized = strings.TrimSuffix(name, "/")
	}
	if normalized == "" || normalized == "." || path.IsAbs(normalized) || filepath.IsAbs(normalized) ||
		hasDrivePrefix(normalized) || path.Clean(normalized) != normalized ||
		normalized == ".." || strings.HasPrefix(normalized, "../") {
		return fmt.Errorf("scanning zip entry %q: path traversal: %w", name, ErrUnsafeArchive)
	}
	mode := file.Mode()
	if mode&fs.ModeSymlink != 0 {
		return fmt.Errorf("scanning zip entry %q: symbolic link: %w", name, ErrUnsafeArchive)
	}
	if !isDirectory && !mode.IsRegular() {
		return fmt.Errorf("scanning zip entry %q: special file: %w", name, ErrUnsafeArchive)
	}

	return nil
}

func validateZIPSizes(file *zip.File, limits ArchiveLimits) (int64, int64, error) {
	if file.UncompressedSize64 > math.MaxInt64 || file.CompressedSize64 > math.MaxInt64 {
		return 0, 0, fmt.Errorf("scanning zip entry %q: size overflow: %w", file.Name, ErrArchiveLimit)
	}
	uncompressed := int64(file.UncompressedSize64)
	compressed := int64(file.CompressedSize64)
	if uncompressed > limits.MaxEntryBytes {
		return 0, 0, fmt.Errorf("scanning zip entry %q: declared size: %w", file.Name, ErrArchiveLimit)
	}
	if exceedsCompressionRatio(uncompressed, compressed, limits.MaxCompressionRatio) {
		return 0, 0, fmt.Errorf("scanning zip entry %q: declared compression ratio: %w", file.Name, ErrArchiveLimit)
	}

	return uncompressed, compressed, nil
}

func exceedsCompressionRatio(uncompressed, compressed int64, maximum float64) bool {
	if uncompressed == 0 {
		return false
	}
	if compressed == 0 {
		return true
	}

	return float64(uncompressed)/float64(compressed) > maximum
}

func classifyZIPEntry(name, extension string) ZIPEntryKind {
	if isPlatformMetadata(name) {
		return ZIPEntryIgnored
	}
	switch extension {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return ZIPEntryImage
	default:
		return ZIPEntryUnhandled
	}
}

func isPlatformMetadata(name string) bool {
	if strings.HasPrefix(name, "__MACOSX/") {
		return true
	}
	base := path.Base(name)

	return base == ".DS_Store" || strings.EqualFold(base, "Thumbs.db")
}

func hasZIPMagic(header []byte) bool {
	if len(header) < 4 || header[0] != 'P' || header[1] != 'K' {
		return false
	}

	return header[2] == 3 && header[3] == 4 ||
		header[2] == 5 && header[3] == 6 ||
		header[2] == 7 && header[3] == 8 ||
		header[2] == 8 && header[3] == 7
}
