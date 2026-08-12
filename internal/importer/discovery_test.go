package importer

import (
	"context"
	"io/fs"
	"testing"
	"time"
)

func TestDiscoverSortsClassifiesAndIgnoresNonRegularEntries(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	snapshot := fakeIncomingSnapshotter{entries: []IncomingEntry{
		{Name: "zeta.TXT", Mode: 0, Size: 9, ModTime: now},
		{Name: "Beta.ZIP", Mode: 0, Size: 8, ModTime: now},
		{Name: "alpha.PNG", Mode: 0, Size: 7, ModTime: now},
		{Name: "directory", Mode: fs.ModeDir, ModTime: now},
		{Name: "link.webp", Mode: fs.ModeSymlink, ModTime: now},
	}}

	discovery, err := Discover(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(discovery.Failures) != 0 {
		t.Fatalf("Discover() failures = %+v", discovery.Failures)
	}
	if len(discovery.Sources) != 3 {
		t.Fatalf("Discover() source count = %d", len(discovery.Sources))
	}
	wantPaths := []string{"alpha.PNG", "Beta.ZIP", "zeta.TXT"}
	wantKinds := []CandidateKind{CandidateKindLoose, CandidateKindZIP, CandidateKindUnsupported}
	for index, source := range discovery.Sources {
		if source.Path.String() != wantPaths[index] || source.Kind != wantKinds[index] {
			t.Errorf("source[%d] = %+v", index, source)
		}
	}
}

func TestDiscoverUsesOriginalNameAsCaseFoldedSortTieBreaker(t *testing.T) {
	t.Parallel()

	discovery, err := Discover(context.Background(), fakeIncomingSnapshotter{entries: []IncomingEntry{
		{Name: "a.PNG", Mode: 0}, {Name: "A.png", Mode: 0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := discovery.Sources[0].Path.String(); got != "A.png" {
		t.Fatalf("first source = %q", got)
	}
}

func TestDiscoverRecordsUnsafeRegularName(t *testing.T) {
	t.Parallel()

	discovery, err := Discover(context.Background(), fakeIncomingSnapshotter{entries: []IncomingEntry{
		{Name: `unsafe\name.png`, Mode: 0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Sources) != 0 || len(discovery.Failures) != 1 {
		t.Fatalf("Discover() = %+v", discovery)
	}
}

func TestDiscoverHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, fakeIncomingSnapshotter{}); err == nil {
		t.Fatal("Discover() error = nil")
	}
}

type fakeIncomingSnapshotter struct {
	entries []IncomingEntry
	err     error
}

func (snapshotter fakeIncomingSnapshotter) SnapshotIncoming(context.Context) ([]IncomingEntry, error) {
	return snapshotter.entries, snapshotter.err
}
