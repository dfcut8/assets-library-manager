package importer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
)

func BenchmarkScanZIP(b *testing.B) {
	entries := make([]zipTestEntry, 50)
	for index := range entries {
		entries[index] = zipTestEntry{
			name: fmt.Sprintf("sprites/%03d.png", index),
			data: bytes.Repeat([]byte{byte(index), 0x89, 'P', 'N', 'G'}, 200),
		}
	}
	archive := makeZIP(b, entries)
	b.ReportAllocs()
	b.SetBytes(int64(len(archive)))
	for b.Loop() {
		_, err := ScanZIP(
			context.Background(), bytes.NewReader(archive), int64(len(archive)), generousArchiveLimits(),
			func(_ context.Context, _ ZIPEntry, reader io.Reader) error {
				_, err := io.Copy(io.Discard, reader)

				return err
			},
		)
		if err != nil {
			b.Fatal(err)
		}
	}
}
