package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
)

func TestScanZIPStreamsImagesAndClassifiesBenignEntries(t *testing.T) {
	t.Parallel()

	archive := makeZIP(t, []zipTestEntry{
		{name: "art/Hero.PNG", data: []byte("\x89PNG\r\n\x1a\nimage")},
		{name: "__MACOSX/._Hero.PNG", data: []byte("metadata")},
		{name: "notes.txt", data: []byte("retain this archive")},
		{name: "empty/", directory: true},
	})
	visited := make([]string, 0)
	report, err := ScanZIP(
		context.Background(), bytes.NewReader(archive), int64(len(archive)), generousArchiveLimits(),
		func(_ context.Context, entry ZIPEntry, reader io.Reader) error {
			data, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			visited = append(visited, entry.Name+":"+string(data[:4]))

			return nil
		},
	)
	if err != nil {
		t.Fatalf("ScanZIP() error = %v", err)
	}
	if report.ImageCount != 1 || report.IgnoredCount != 1 || report.UnhandledCount != 1 ||
		report.DirectoryCount != 1 || len(visited) != 1 || visited[0] != "art/Hero.PNG:\x89PNG" {
		t.Fatalf("ScanZIP() report = %+v visited = %v", report, visited)
	}
}

func TestScanZIPRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "traversal", path: "../escape.png"},
		{name: "absolute", path: "/escape.png"},
		{name: "drive", path: "C:/escape.png"},
		{name: "backslash", path: `..\escape.png`},
		{name: "unc", path: `\\server\escape.png`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			archive := makeZIP(t, []zipTestEntry{{name: test.path, data: []byte("image")}})
			_, err := ScanZIP(
				context.Background(), bytes.NewReader(archive), int64(len(archive)),
				generousArchiveLimits(), nil,
			)
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("ScanZIP() error = %v", err)
			}
		})
	}
}

func TestScanZIPRejectsLinksSpecialFilesAndNestedArchives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry zipTestEntry
	}{
		{name: "symlink", entry: zipTestEntry{name: "link.png", data: []byte("target"), mode: os.ModeSymlink | 0o777}},
		{name: "special", entry: zipTestEntry{name: "socket.png", mode: os.ModeSocket | 0o600}},
		{name: "nested extension", entry: zipTestEntry{name: "nested.ZIP", data: []byte("not even zip")}},
		{name: "renamed nested magic", entry: zipTestEntry{name: "nested.bin", data: []byte("PK\x03\x04payload")}},
		{name: "image nested magic", entry: zipTestEntry{name: "nested.png", data: []byte("PK\x05\x06payload")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			archive := makeZIP(t, []zipTestEntry{test.entry})
			_, err := ScanZIP(
				context.Background(), bytes.NewReader(archive), int64(len(archive)),
				generousArchiveLimits(), func(context.Context, ZIPEntry, io.Reader) error { return nil },
			)
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("ScanZIP() error = %v", err)
			}
		})
	}
}

func TestScanZIPEnforcesDeclaredLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limits ArchiveLimits
	}{
		{name: "entry count", limits: ArchiveLimits{MaxEntries: 1, MaxEntryBytes: 100, MaxTotalUncompressedBytes: 100, MaxCompressionRatio: 1000}},
		{name: "entry size", limits: ArchiveLimits{MaxEntries: 2, MaxEntryBytes: 3, MaxTotalUncompressedBytes: 100, MaxCompressionRatio: 1000}},
		{name: "total size", limits: ArchiveLimits{MaxEntries: 2, MaxEntryBytes: 100, MaxTotalUncompressedBytes: 7, MaxCompressionRatio: 1000}},
		{name: "compression ratio", limits: ArchiveLimits{MaxEntries: 2, MaxEntryBytes: 100, MaxTotalUncompressedBytes: 100, MaxCompressionRatio: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			archive := makeZIP(t, []zipTestEntry{
				{name: "one.png", data: bytes.Repeat([]byte("a"), 50)},
				{name: "two.png", data: bytes.Repeat([]byte("b"), 50)},
			})
			_, err := ScanZIP(
				context.Background(), bytes.NewReader(archive), int64(len(archive)), test.limits,
				func(context.Context, ZIPEntry, io.Reader) error { return nil },
			)
			if !errors.Is(err, ErrArchiveLimit) {
				t.Fatalf("ScanZIP() error = %v", err)
			}
		})
	}
}

func TestScanZIPDetectsLyingDeclaredSize(t *testing.T) {
	t.Parallel()

	archive := makeZIP(t, []zipTestEntry{{name: "image.png", data: []byte("0123456789")}})
	centralDirectory := bytes.Index(archive, []byte("PK\x01\x02"))
	if centralDirectory < 0 {
		t.Fatal("central directory not found")
	}
	binary.LittleEndian.PutUint32(archive[centralDirectory+24:centralDirectory+28], 1)
	limits := generousArchiveLimits()
	limits.MaxEntryBytes = 4
	_, err := ScanZIP(
		context.Background(), bytes.NewReader(archive), int64(len(archive)), limits,
		func(context.Context, ZIPEntry, io.Reader) error { return nil },
	)
	if !errors.Is(err, ErrTruncatedArchive) {
		t.Fatalf("ScanZIP() error = %v", err)
	}
}

func TestScanZIPDetectsCorruptEntryStream(t *testing.T) {
	t.Parallel()

	archive := makeZIP(t, []zipTestEntry{{name: "image.png", data: []byte("uncompressible-ish-123456")}})
	localHeader := bytes.Index(archive, []byte("PK\x03\x04"))
	if localHeader < 0 {
		t.Fatal("local header not found")
	}
	nameLength := int(binary.LittleEndian.Uint16(archive[localHeader+26 : localHeader+28]))
	extraLength := int(binary.LittleEndian.Uint16(archive[localHeader+28 : localHeader+30]))
	dataOffset := localHeader + 30 + nameLength + extraLength
	archive[dataOffset] ^= 0xff
	_, err := ScanZIP(
		context.Background(), bytes.NewReader(archive), int64(len(archive)), generousArchiveLimits(),
		func(_ context.Context, _ ZIPEntry, reader io.Reader) error {
			_, err := io.Copy(io.Discard, reader)

			return err
		},
	)
	if !errors.Is(err, ErrTruncatedArchive) {
		t.Fatalf("ScanZIP() error = %v", err)
	}
}

func TestScanZIPDrainsPartiallyConsumedImage(t *testing.T) {
	t.Parallel()

	data := []byte("\x89PNG\r\n\x1a\nremaining bytes")
	archive := makeZIP(t, []zipTestEntry{{name: "image.png", data: data}})
	report, err := ScanZIP(
		context.Background(), bytes.NewReader(archive), int64(len(archive)), generousArchiveLimits(),
		func(_ context.Context, _ ZIPEntry, reader io.Reader) error {
			buffer := make([]byte, 4)
			_, err := io.ReadFull(reader, buffer)

			return err
		},
	)
	if err != nil {
		t.Fatalf("ScanZIP() error = %v", err)
	}
	if report.ActualStreamBytes != int64(len(data)) {
		t.Fatalf("actual bytes = %d", report.ActualStreamBytes)
	}
}

func FuzzScanZIPDoesNotPanic(f *testing.F) {
	f.Add([]byte("not a zip"))
	f.Add(makeZIP(f, []zipTestEntry{{name: "image.png", data: []byte("image")}}))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ScanZIP(
			context.Background(), bytes.NewReader(data), int64(len(data)), generousArchiveLimits(),
			func(context.Context, ZIPEntry, io.Reader) error { return nil },
		)
	})
}

type zipTestEntry struct {
	name      string
	data      []byte
	mode      os.FileMode
	directory bool
}

type testFataler interface {
	Helper()
	Fatal(args ...any)
}

func makeZIP(t testFataler, entries []zipTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.directory {
			header.SetMode(os.ModeDir | 0o700)
			header.Method = zip.Store
		} else if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		member, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

func generousArchiveLimits() ArchiveLimits {
	return ArchiveLimits{
		MaxEntries: 100, MaxEntryBytes: 1 << 20,
		MaxTotalUncompressedBytes: 10 << 20, MaxCompressionRatio: 10_000,
	}
}
