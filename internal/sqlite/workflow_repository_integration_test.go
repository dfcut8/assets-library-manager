//go:build integration

package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/importer"
)

func TestWorkflowWriterGateAllowsWALReadsAndHonorsQueuedCancellation(t *testing.T) {
	database := openWorkflowTestDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	source := testSourceRecord(t, now)
	if _, err := database.CreateImportSource(ctx, source); err != nil {
		t.Fatal(err)
	}

	writerStarted := make(chan struct{})
	releaseWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- database.withWriteTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				UPDATE import_sources SET state = 'processing' WHERE id = ?
			`, source.ID.String()); err != nil {
				return err
			}
			close(writerStarted)
			<-releaseWriter

			return nil
		})
	}()
	<-writerStarted

	read, err := database.FindImportSourceByPath(ctx, source.Path)
	if err != nil {
		t.Fatalf("WAL read during write error = %v", err)
	}
	if read.State != importer.SourceStateDiscovered {
		t.Fatalf("WAL read observed uncommitted state %q", read.State)
	}

	queuedCtx, cancelQueued := context.WithCancel(ctx)
	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- database.withWriteTx(queuedCtx, func(*sql.Tx) error {
			return errors.New("canceled queued writer entered transaction")
		})
	}()
	cancelQueued()
	close(releaseWriter)
	if err := <-writerDone; err != nil {
		t.Fatalf("first writer error = %v", err)
	}
	if err := <-queuedDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued writer error = %v, want context canceled", err)
	}
}

func TestWorkflowRepositorySourceIdentityAndTransitions(t *testing.T) {
	database := openWorkflowTestDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	source := testSourceRecord(t, now)

	created, err := database.CreateImportSource(ctx, source)
	if err != nil {
		t.Fatalf("CreateImportSource() error = %v", err)
	}
	if created != source {
		t.Fatalf("CreateImportSource() = %+v, want %+v", created, source)
	}
	repeated, err := database.CreateImportSource(ctx, source)
	if err != nil || repeated.ID != source.ID {
		t.Fatalf("CreateImportSource(repeated) = %+v, %v", repeated, err)
	}
	changed := source
	changed.ID = integrationID(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	changed.DiscoveryFingerprint = importer.NewDigest(sha256.Sum256([]byte("replacement")))
	retained, err := database.CreateImportSource(ctx, changed)
	if !errors.Is(err, importer.ErrSourceChanged) {
		t.Fatalf("CreateImportSource(changed) error = %v", err)
	}
	if retained.State != importer.SourceStateRetained || retained.ErrorCode != importer.ErrorCodeSourceChanged {
		t.Fatalf("CreateImportSource(changed) = %+v", retained)
	}
	found, err := database.FindImportSourceByPath(ctx, source.Path)
	if err != nil || found.ID != source.ID || found.DiscoveryFingerprint != source.DiscoveryFingerprint ||
		found.State != importer.SourceStateRetained {
		t.Fatalf("FindImportSourceByPath() = %+v, %v", found, err)
	}

	item := importer.ItemRecord{
		ID: integrationID(t, "cccccccccccccccccccccccccccccccc"), SourceID: source.ID,
		State: importer.ItemStateDiscovered, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := database.CreateImportItem(ctx, item); err != nil {
		t.Fatalf("CreateImportItem() error = %v", err)
	}
	stagedPath, err := importer.NewStagedPath(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest := importer.NewDigest(sha256.Sum256([]byte("image bytes")))
	if err := database.TransitionImportItem(ctx, importer.ItemTransition{
		ID: item.ID, From: importer.ItemStateDiscovered, To: importer.ItemStateStaged,
		StagedPath: stagedPath, Digest: digest, UpdatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("TransitionImportItem() error = %v", err)
	}
	if err := database.TransitionImportItem(ctx, importer.ItemTransition{
		ID: item.ID, From: importer.ItemStateDiscovered, To: importer.ItemStateStaged,
		StagedPath: stagedPath, Digest: digest, UpdatedAt: now.Add(2 * time.Second),
	}); !errors.Is(err, importer.ErrInvalidTransition) {
		t.Fatalf("stale TransitionImportItem() error = %v", err)
	}
	stored, err := database.FindImportItem(ctx, item.ID)
	if err != nil || stored.State != importer.ItemStateStaged || stored.Digest != digest || stored.StagedPath != stagedPath {
		t.Fatalf("FindImportItem() = %+v, %v", stored, err)
	}
	missing := integrationID(t, "dddddddddddddddddddddddddddddddd")
	if _, err := database.FindImportItem(ctx, missing); !errors.Is(err, importer.ErrNotFound) {
		t.Fatalf("FindImportItem(missing) error = %v", err)
	}
	summary, err := database.SummarizeSource(ctx, source.ID)
	if err != nil || summary.Total != 1 || summary.Staged != 1 {
		t.Fatalf("SummarizeSource() = %+v, %v", summary, err)
	}
	if _, err := database.SummarizeSource(ctx, missing); !errors.Is(err, importer.ErrNotFound) {
		t.Fatalf("SummarizeSource(missing) error = %v", err)
	}
	recoverable, err := database.ListRecoverableItems(ctx)
	if err != nil || len(recoverable) != 1 || recoverable[0].ID != item.ID {
		t.Fatalf("ListRecoverableItems() = %+v, %v", recoverable, err)
	}
}

func TestWorkflowRepositoryCommitReadyAndRecoveryQueries(t *testing.T) {
	database := openWorkflowTestDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	source := testSourceRecord(t, now)
	source.DeletionState = importer.DeletionStatePending
	if _, err := database.CreateImportSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	item := importer.ItemRecord{
		ID: integrationID(t, "cccccccccccccccccccccccccccccccc"), SourceID: source.ID,
		State: importer.ItemStateDiscovered, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := database.CreateImportItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	stagedPath, err := importer.NewStagedPath(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest := importer.NewDigest(sha256.Sum256([]byte("image bytes")))
	if err := database.TransitionImportItem(ctx, importer.ItemTransition{
		ID: item.ID, From: importer.ItemStateDiscovered, To: importer.ItemStateStaged,
		StagedPath: stagedPath, Digest: digest, UpdatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.TransitionImportItem(ctx, importer.ItemTransition{
		ID: item.ID, From: importer.ItemStateStaged, To: importer.ItemStateAnalyzing,
		UpdatedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	asset := testStagedAsset(t, item.ID, digest, now.Add(3*time.Second))
	if err := database.CommitStagedAsset(ctx, asset); err != nil {
		t.Fatalf("CommitStagedAsset() error = %v", err)
	}
	if _, err := database.FindReadyByDigest(ctx, digest); !errors.Is(err, importer.ErrNotFound) {
		t.Fatalf("FindReadyByDigest(staged) error = %v", err)
	}
	recoveryAssets, err := database.ListRecoveryAssets(ctx)
	if err != nil {
		t.Fatalf("ListRecoveryAssets() error = %v", err)
	}
	if len(recoveryAssets) != 1 || recoveryAssets[0].ID != asset.ID || recoveryAssets[0].StagedPath != stagedPath {
		t.Fatalf("ListRecoveryAssets() = %+v", recoveryAssets)
	}
	references, err := database.ListReferencedStagedPaths(ctx)
	if err != nil || len(references) != 1 || references[0] != stagedPath {
		t.Fatalf("ListReferencedStagedPaths() = %v, %v", references, err)
	}
	deletions, err := database.ListPendingDeletions(ctx)
	if err != nil || len(deletions) != 1 || deletions[0].ID != source.ID {
		t.Fatalf("ListPendingDeletions() = %+v, %v", deletions, err)
	}

	readyAt := now.Add(4 * time.Second)
	if err := database.MarkAssetReady(ctx, asset.ID, item.ID, readyAt); err != nil {
		t.Fatalf("MarkAssetReady() error = %v", err)
	}
	ready, err := database.FindReadyByDigest(ctx, digest)
	if err != nil || ready.ID != asset.ID || ready.ManagedPath != asset.ManagedPath {
		t.Fatalf("FindReadyByDigest() = %+v, %v", ready, err)
	}
	recoveryAssets, err = database.ListRecoveryAssets(ctx)
	if err != nil || len(recoveryAssets) != 1 || recoveryAssets[0].State != importer.AssetStateReady {
		t.Fatalf("ListRecoveryAssets(ready) = %+v, %v", recoveryAssets, err)
	}
	if err := database.MarkAssetIntegrityFailed(ctx, asset.ID, now.Add(5*time.Second)); err != nil {
		t.Fatalf("MarkAssetIntegrityFailed() error = %v", err)
	}
	if _, err := database.FindReadyByDigest(ctx, digest); !errors.Is(err, importer.ErrNotFound) {
		t.Fatalf("FindReadyByDigest(integrity failed) error = %v", err)
	}
}

func TestWorkflowRepositoryFailStagedAssetIsTransactional(t *testing.T) {
	database := openWorkflowTestDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	source := testSourceRecord(t, now)
	if _, err := database.CreateImportSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	item := importer.ItemRecord{
		ID: integrationID(t, "cccccccccccccccccccccccccccccccc"), SourceID: source.ID,
		State: importer.ItemStateDiscovered, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := database.CreateImportItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	stagedPath, _ := importer.NewStagedPath(item.ID)
	digest := importer.NewDigest(sha256.Sum256([]byte("image bytes")))
	if err := database.TransitionImportItem(ctx, importer.ItemTransition{
		ID: item.ID, From: importer.ItemStateDiscovered, To: importer.ItemStateStaged,
		StagedPath: stagedPath, Digest: digest, UpdatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.TransitionImportItem(ctx, importer.ItemTransition{
		ID: item.ID, From: importer.ItemStateStaged, To: importer.ItemStateAnalyzing,
		UpdatedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	asset := testStagedAsset(t, item.ID, digest, now.Add(3*time.Second))
	if err := database.CommitStagedAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	if err := database.FailStagedAsset(
		ctx, asset.ID, item.ID, importer.ErrorCodeIntegrity, "both durable copies are missing", now.Add(4*time.Second),
	); err != nil {
		t.Fatalf("FailStagedAsset() error = %v", err)
	}
	stored, err := database.FindImportItem(ctx, item.ID)
	if err != nil || stored.State != importer.ItemStateFailed || !stored.AssetID.IsZero() {
		t.Fatalf("FindImportItem() = %+v, %v", stored, err)
	}
	var assetCount int
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM assets WHERE id = ?", asset.ID.String()).Scan(&assetCount); err != nil {
		t.Fatal(err)
	}
	if assetCount != 0 {
		t.Fatalf("lost staged asset count = %d", assetCount)
	}
}

func openWorkflowTestDatabase(t *testing.T) *Database {
	t.Helper()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "assets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Database.Close() error = %v", err)
		}
	})

	return database
}

func testSourceRecord(t *testing.T, now time.Time) importer.SourceRecord {
	t.Helper()
	sourcePath, err := importer.NewSourcePath("incoming.png")
	if err != nil {
		t.Fatal(err)
	}

	return importer.SourceRecord{
		ID: integrationID(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Path: sourcePath,
		Type: importer.SourceTypeLoose, DiscoveryFingerprint: importer.NewDigest(sha256.Sum256([]byte("source"))),
		State: importer.SourceStateDiscovered, DeletionState: importer.DeletionStateNotEligible,
		DiscoveredAt: now, UpdatedAt: now,
	}
}

func testStagedAsset(
	t *testing.T,
	itemID importer.ID,
	digest importer.Digest,
	now time.Time,
) importer.StagedAsset {
	t.Helper()
	managedPath, err := importer.NewManagedPath("other/image--123456789abc.png")
	if err != nil {
		t.Fatal(err)
	}
	completedAt := now

	return importer.StagedAsset{
		ID: integrationID(t, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"), ItemID: itemID,
		Digest: digest, OriginalFilename: "incoming.png", ManagedPath: managedPath,
		Format: "png", MIMEType: "image/png", FileSizeBytes: 11,
		DisplayWidth: 16, DisplayHeight: 16, OrientationClass: "square",
		HasAlpha: true, HasTransparency: true, EncodedFrameCount: 1,
		DominantColorsJSON: `[{"hex":"#112233","count":10}]`, Title: "Imported image",
		Description: "A test image.", PrimaryType: catalog.PrimaryTypeOther, Style: "pixel art",
		PixelArt: true, AIConfidence: 0.9, Layout: catalog.Layout{Kind: catalog.LayoutKindSingle},
		SearchTags: "test", Thumbnail: importer.Thumbnail{Width: 16, Height: 16, Data: []byte("png")},
		Tags: []importer.Tag{{Facet: "subject", Slug: "test", Label: "Test", Origin: "ai"}},
		AIRun: importer.AIRun{
			ID: integrationID(t, "ffffffffffffffffffffffffffffffff"), Provider: "openai",
			Model: "test-model", ReasoningEffort: "medium", ImageDetail: "high",
			PromptVersion: "v1", SchemaVersion: "v1", AttemptNumber: 1,
			StartedAt: now.Add(-time.Second), CompletedAt: &completedAt, Latency: time.Second,
			Outcome: "accepted", NormalizedResultJSON: `{"title":"Imported image"}`,
		},
		CreatedAt: now,
	}
}

func integrationID(t *testing.T, value string) importer.ID {
	t.Helper()
	id, err := importer.ParseID(value)
	if err != nil {
		t.Fatal(err)
	}

	return id
}
