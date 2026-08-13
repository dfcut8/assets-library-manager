//go:build integration

package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
)

func TestCatalogRepositorySearchFiltersPagesAndExcludesNonReadyAssets(t *testing.T) {
	database := openWorkflowTestDatabase(t)
	ctx := context.Background()
	baseTime := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	knightID := catalogAssetID(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	castleID := catalogAssetID(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	insertCatalogFixture(t, database, catalogFixture{
		ID: knightID, DigestByte: 1, Title: "Blue Knight", Description: "A heroic knight.",
		PrimaryType: catalog.PrimaryTypeCharacter, Style: "Pixel Art",
		Width: 64, Height: 32, Format: "png", PixelArt: true, Transparency: true,
		State: "ready", ImportedAt: baseTime.Add(time.Hour),
	})
	insertCatalogFixture(t, database, catalogFixture{
		ID: castleID, DigestByte: 2, Title: "Blue Castle", Description: "A painted fortress.",
		PrimaryType: catalog.PrimaryTypeBuilding, Style: "Painted",
		Width: 128, Height: 128, Format: "jpeg",
		State: "ready", ImportedAt: baseTime,
	})
	insertCatalogFixture(t, database, catalogFixture{
		ID:         catalogAssetID(t, "cccccccccccccccccccccccccccccccc"),
		DigestByte: 3, Title: "Hidden Blue Asset", Description: "Damaged.",
		PrimaryType: catalog.PrimaryTypeOther, Style: "Other", Width: 16, Height: 16,
		Format: "png", State: "integrity-failed", ImportedAt: baseTime,
	})
	insertCatalogFixture(t, database, catalogFixture{
		ID:         catalogAssetID(t, "dddddddddddddddddddddddddddddddd"),
		DigestByte: 4, Title: "Staged Blue Asset", Description: "Not committed.",
		PrimaryType: catalog.PrimaryTypeOther, Style: "Other", Width: 16, Height: 16,
		Format: "png", State: "staged", ImportedAt: baseTime,
	})
	linkCatalogTag(t, database, knightID, "subject", "hero", "Hero", "ai", baseTime)
	linkCatalogTag(t, database, knightID, "theme", "fantasy", "Fantasy", "ai", baseTime)
	linkCatalogTag(t, database, castleID, "theme", "fantasy", "Fantasy", "ai", baseTime)

	page, err := database.Search(ctx, catalog.AssetQuery{Q: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalItems != 2 || len(page.Items) != 2 || page.Items[0].ID != knightID {
		t.Fatalf("blue search = %+v", page)
	}
	trueValue := true
	page, err = database.Search(ctx, catalog.AssetQuery{
		Types:    []catalog.PrimaryType{catalog.PrimaryTypeCharacter, catalog.PrimaryTypeBuilding},
		Styles:   []string{"pixel art", "painted"},
		Tags:     []catalog.TagFilter{{Facet: "subject", Slug: "hero"}},
		PixelArt: &trueValue, MinWidth: 32, Formats: []string{"png", "jpeg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalItems != 1 || page.Items[0].ID != knightID || len(page.Items[0].Tags) != 2 {
		t.Fatalf("combined filters = %+v", page)
	}
	page, err = database.Search(ctx, catalog.AssetQuery{
		Tags: []catalog.TagFilter{
			{Facet: "subject", Slug: "hero"},
			{Facet: "theme", Slug: "fantasy"},
		},
	})
	if err != nil || page.TotalItems != 2 {
		t.Fatalf("OR tag group = %+v, %v", page, err)
	}
	falseValue := false
	from := baseTime.Add(30 * time.Minute)
	page, err = database.Search(ctx, catalog.AssetQuery{
		Transparency: &falseValue, ImportedTo: &from,
		Orientations: []string{"square"}, MaxWidth: 128,
	})
	if err != nil || page.TotalItems != 1 || page.Items[0].ID != castleID {
		t.Fatalf("technical and date filters = %+v, %v", page, err)
	}
	page, err = database.Search(ctx, catalog.AssetQuery{
		Sort: catalog.SortTitle, PageSize: 1, Page: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalItems != 2 || page.TotalPages != 2 || len(page.Items) != 1 || page.Items[0].ID != knightID {
		t.Fatalf("second title page = %+v", page)
	}
	page, err = database.Search(ctx, catalog.AssetQuery{Q: "blue OR castle"})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalItems != 0 {
		t.Fatalf("raw FTS operator was interpreted: %+v", page)
	}
}

func TestCatalogRepositoryDetailLookupsAndLatestProvenance(t *testing.T) {
	database := openWorkflowTestDatabase(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	assetID := catalogAssetID(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	insertCatalogFixture(t, database, catalogFixture{
		ID: assetID, DigestByte: 9, Title: "Detail Asset", Description: "Detailed description.",
		PrimaryType: catalog.PrimaryTypeProp, Style: "Painted", Width: 64, Height: 64,
		Format: "webp", State: "ready", ImportedAt: now,
	})
	linkCatalogTag(t, database, assetID, "material", "wood", "Wood", "ai", now)
	insertCatalogAIRuns(t, database, assetID, now)

	detail, err := database.Get(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Title != "Detail Asset" || detail.SHA256 == "" || len(detail.Tags) != 1 ||
		string(detail.Thumbnail.Data) != "png-a" ||
		detail.AI == nil || detail.AI.AttemptNumber != 2 || detail.AI.Model != "new-model" {
		t.Fatalf("detail = %+v", detail)
	}
	thumbnail, err := database.GetThumbnail(ctx, assetID)
	if err != nil || string(thumbnail.Data) != "png-a" || thumbnail.Version != 1 {
		t.Fatalf("thumbnail = %+v, %v", thumbnail, err)
	}
	original, err := database.GetOriginal(ctx, assetID)
	if err != nil || original.ManagedPath == "" || len(original.SHA256) != 64 {
		t.Fatalf("original = %+v, %v", original, err)
	}
	missing := catalogAssetID(t, "ffffffffffffffffffffffffffffffff")
	if _, err := database.Get(ctx, missing); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("Get(missing) error = %v", err)
	}
}

func TestCatalogRepositoryMetadataReplacementUpdatesFTSAndRejectsStaleEdits(t *testing.T) {
	database := openWorkflowTestDatabase(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	assetID := catalogAssetID(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	insertCatalogFixture(t, database, catalogFixture{
		ID: assetID, DigestByte: 7, Title: "Old Crate", Description: "Legacy obsolete metadata.",
		PrimaryType: catalog.PrimaryTypeProp, Style: "Pixel Art", Width: 64, Height: 64,
		Format: "png", State: "ready", ImportedAt: now,
	})
	linkCatalogTag(t, database, assetID, "subject", "old-crate", "Old crate", "ai", now)
	edit := catalog.MetadataEdit{
		Version: 1, Title: "Updated Tile Collection", Description: "Corrected metadata.",
		PrimaryType: catalog.PrimaryTypeEnvironment, Style: "Hand painted", PixelArt: false,
		Layout: catalog.Layout{
			Kind: catalog.LayoutKindTileSheet, Columns: 2, Rows: 2,
			CellWidth: 32, CellHeight: 32,
		},
		Tags: []catalog.Tag{{Facet: "material", Slug: "stone", Label: "Stone"}},
	}
	version, err := database.UpdateSemanticMetadata(ctx, assetID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("updated version = %d", version)
	}
	detail, err := database.Get(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Version != 2 || detail.Layout.Kind != catalog.LayoutKindTileSheet ||
		len(detail.Tags) != 1 || detail.Tags[0].Origin != "user" {
		t.Fatalf("updated detail = %+v", detail)
	}
	newSearch, err := database.Search(ctx, catalog.AssetQuery{Q: "updated stone"})
	if err != nil || newSearch.TotalItems != 1 {
		t.Fatalf("updated FTS = %+v, %v", newSearch, err)
	}
	oldSearch, err := database.Search(ctx, catalog.AssetQuery{Q: "legacy obsolete"})
	if err != nil || oldSearch.TotalItems != 0 {
		t.Fatalf("old FTS = %+v, %v", oldSearch, err)
	}
	singleEdit := edit
	singleEdit.Version = 2
	singleEdit.Layout = catalog.Layout{Kind: catalog.LayoutKindSingle}
	version, err = database.UpdateSemanticMetadata(ctx, assetID, singleEdit)
	if err != nil || version != 3 {
		t.Fatalf("sheet to single update = %d, %v", version, err)
	}
	singleDetail, err := database.Get(ctx, assetID)
	if err != nil || singleDetail.Layout.Kind != catalog.LayoutKindSingle {
		t.Fatalf("single detail = %+v, %v", singleDetail, err)
	}
	if _, err := database.UpdateSemanticMetadata(ctx, assetID, edit); !errors.Is(err, catalog.ErrStaleEdit) {
		t.Fatalf("stale update error = %v", err)
	}
	invalid := edit
	invalid.Version = 3
	invalid.Layout = catalog.Layout{Kind: catalog.LayoutKindTileSheet, Columns: 2}
	if _, err := database.UpdateSemanticMetadata(ctx, assetID, invalid); !errors.Is(err, catalog.ErrInvalid) {
		t.Fatalf("invalid layout error = %v", err)
	}
	afterInvalid, err := database.Get(ctx, assetID)
	if err != nil || afterInvalid.Version != 3 || afterInvalid.Title != detail.Title {
		t.Fatalf("invalid update changed asset = %+v, %v", afterInvalid, err)
	}
}

type catalogFixture struct {
	ID           catalog.AssetID
	DigestByte   byte
	Title        string
	Description  string
	PrimaryType  catalog.PrimaryType
	Style        string
	Width        int
	Height       int
	Format       string
	PixelArt     bool
	Transparency bool
	State        string
	ImportedAt   time.Time
}

func insertCatalogFixture(t *testing.T, database *Database, fixture catalogFixture) {
	t.Helper()
	mimeTypes := map[string]string{
		"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp", "gif": "image/gif",
	}
	orientation := "square"
	if fixture.Width > fixture.Height {
		orientation = "landscape"
	} else if fixture.Height > fixture.Width {
		orientation = "portrait"
	}
	importedAt := any(formatTime(fixture.ImportedAt))
	if fixture.State == "staged" {
		importedAt = nil
	}
	_, err := database.db.ExecContext(context.Background(), `
		INSERT INTO assets(
			id, sha256, original_filename, managed_path, format, mime_type,
			file_size_bytes, display_width, display_height, orientation_class,
			has_alpha, has_transparency, encoded_animated, encoded_frame_count,
			dominant_colors_json, title, description, primary_type, style,
			pixel_art, ai_confidence, layout_kind, search_tags, state, version,
			imported_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, 128, ?, ?, ?, ?, ?, 0, 1, ?, ?, ?, ?, ?, ?, 0.91,
			'single', '', ?, 1, ?, ?, ?)
	`,
		fixture.ID.String(), bytes.Repeat([]byte{fixture.DigestByte}, 32),
		fixture.Title+".png", string(fixture.PrimaryType)+"/"+fixture.ID.String()+"."+fixture.Format,
		fixture.Format, mimeTypes[fixture.Format], fixture.Width, fixture.Height, orientation,
		boolInt(fixture.Transparency), boolInt(fixture.Transparency),
		`[{"hex":"#112233","samples":10}]`, fixture.Title, fixture.Description,
		string(fixture.PrimaryType), fixture.Style, boolInt(fixture.PixelArt), fixture.State,
		importedAt, formatTime(fixture.ImportedAt), formatTime(fixture.ImportedAt),
	)
	if err != nil {
		t.Fatalf("inserting catalog fixture: %v", err)
	}
	if _, err := database.db.ExecContext(context.Background(), `
		INSERT INTO thumbnails(asset_id, mime_type, width, height, byte_length, data)
		VALUES(?, 'image/png', 1, 1, 5, ?)
	`, fixture.ID.String(), []byte("png-a")); err != nil {
		t.Fatalf("inserting catalog thumbnail: %v", err)
	}
}

func linkCatalogTag(
	t *testing.T,
	database *Database,
	assetID catalog.AssetID,
	facet, slug, label, origin string,
	createdAt time.Time,
) {
	t.Helper()
	var tagID int64
	if err := database.db.QueryRowContext(context.Background(), `
		INSERT INTO tags(facet, slug, label) VALUES(?, ?, ?)
		ON CONFLICT(facet, slug) DO UPDATE SET label = excluded.label RETURNING id
	`, facet, slug, label).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(context.Background(), `
		INSERT INTO asset_tags(asset_id, tag_id, origin, created_at) VALUES(?, ?, ?, ?)
	`, assetID.String(), tagID, origin, formatTime(createdAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(context.Background(), `
		UPDATE assets SET search_tags = search_tags || ' ' || ? WHERE id = ?
	`, fmt.Sprintf("%s %s %s", facet, slug, label), assetID.String()); err != nil {
		t.Fatal(err)
	}
}

func insertCatalogAIRuns(t *testing.T, database *Database, assetID catalog.AssetID, now time.Time) {
	t.Helper()
	ctx := context.Background()
	sourceID := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	itemID := "ffffffffffffffffffffffffffffffff"
	if _, err := database.db.ExecContext(ctx, `
		INSERT INTO import_sources(
			id, source_path, source_type, discovery_fingerprint, state,
			deletion_state, discovered_at, updated_at
		) VALUES(?, 'detail.webp', 'loose', 'fingerprint', 'deleted', 'deleted', ?, ?)
	`, sourceID, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `
		INSERT INTO import_items(id, source_id, asset_id, state, created_at, updated_at)
		VALUES(?, ?, ?, 'ready', ?, ?)
	`, itemID, sourceID, assetID.String(), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	for attempt, model := range []string{"old-model", "new-model"} {
		runID := fmt.Sprintf("%032x", attempt+1)
		startedAt := now.Add(time.Duration(attempt) * time.Second)
		if _, err := database.db.ExecContext(ctx, `
			INSERT INTO ai_runs(
				id, import_item_id, asset_id, provider, model, reasoning_effort,
				image_detail, prompt_version, schema_version, attempt_number,
				started_at, completed_at, latency_ms, outcome
			) VALUES(?, ?, ?, 'openai', ?, 'medium', 'auto', 'v1', 'v1', ?, ?, ?, 10, 'accepted')
		`, runID, itemID, assetID.String(), model, attempt+1, formatTime(startedAt), formatTime(startedAt)); err != nil {
			t.Fatal(err)
		}
	}
}

func catalogAssetID(t *testing.T, value string) catalog.AssetID {
	t.Helper()
	id, err := catalog.ParseAssetID(value)
	if err != nil {
		t.Fatal(err)
	}

	return id
}
