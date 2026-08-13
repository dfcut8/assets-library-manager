package importer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
)

func TestManagedAssetPathUsesContentFormatAndStableDigestSuffix(t *testing.T) {
	t.Parallel()
	digest, err := ParseDigest(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	managedPath, err := managedAssetPath(AnalysisResult{
		Title: "CON / Élite Hero!", PrimaryType: catalog.PrimaryTypeCharacter,
	}, "jpeg", digest)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := managedPath.String(), "character/con-lite-hero--abababababab.jpg"; got != want {
		t.Fatalf("managed path = %q, want %q", got, want)
	}
}

func TestStopReservationsReleasesWaitersAndRejectsNewWork(t *testing.T) {
	t.Parallel()
	coordinator := &Coordinator{reservations: make(map[Digest]*reservation)}
	digest, err := ParseDigest(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	leader, _, err := coordinator.reserve(context.Background(), digest)
	if err != nil || !leader {
		t.Fatalf("first reserve = %v, %v", leader, err)
	}
	waiterDone := make(chan error, 1)
	go func() {
		_, _, err := coordinator.reserve(context.Background(), digest)
		waiterDone <- err
	}()
	coordinator.StopReservations()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reservation waiter was not released")
	}
	if _, _, err := coordinator.reserve(context.Background(), digest); !errors.Is(err, context.Canceled) {
		t.Fatalf("reserve after stop error = %v", err)
	}
}

func TestProgressSnapshotIsDefensive(t *testing.T) {
	t.Parallel()
	coordinator := &Coordinator{progress: Progress{
		Failures: []ProgressFailure{{Source: "asset.png", Message: "retained"}},
	}}
	snapshot := coordinator.Snapshot()
	snapshot.Failures[0].Message = "mutated"
	if got := coordinator.Snapshot().Failures[0].Message; got != "retained" {
		t.Fatalf("stored failure changed to %q", got)
	}
}

func TestFailedResultLogsWrappedCauseAtDebugLevel(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	coordinator := &Coordinator{
		repository: transitionRepository{},
		logger: slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
		now: time.Now,
	}
	sourcePath, err := NewSourcePath("r8_buildings_adventure.png")
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("inserting staged asset: constraint failed")
	coordinator.failedResult(
		context.Background(),
		workItem{sourcePath: sourcePath},
		ItemRecord{},
		ErrorCodeStorage,
		"asset metadata could not be committed",
		cause,
	)

	got := output.String()
	if !strings.Contains(got, "import item processing failure details") ||
		!strings.Contains(got, "inserting staged asset: constraint failed") {
		t.Fatalf("debug log output = %q, want wrapped storage cause", got)
	}
}

type transitionRepository struct {
	WorkflowRepository
}

func (transitionRepository) TransitionImportItem(context.Context, ItemTransition) error {
	return nil
}
