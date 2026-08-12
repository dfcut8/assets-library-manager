package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/importer"
)

// CommitStagedAsset atomically persists a fully analyzed staged asset and advances its item.
func (d *Database) CommitStagedAsset(ctx context.Context, asset importer.StagedAsset) error {
	if err := validateStagedAsset(asset); err != nil {
		return err
	}
	return d.withWriteTx(ctx, func(tx *sql.Tx) error {
		var conflicts int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM assets WHERE sha256 = ? OR managed_path = ?
		`, asset.Digest.Bytes(), asset.ManagedPath.String()).Scan(&conflicts); err != nil {
			return fmt.Errorf("checking staged asset conflict: %w", err)
		}
		if conflicts != 0 {
			return importer.ErrConflict
		}
		createdAt := formatTime(asset.CreatedAt)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO assets(
				id, sha256, original_filename, managed_path, format, mime_type,
				file_size_bytes, display_width, display_height, orientation_class,
				has_alpha, has_transparency, encoded_animated, encoded_frame_count,
				dominant_colors_json, title, description, primary_type, style,
				pixel_art, ai_confidence, layout_kind, search_tags, state, version,
				imported_at, created_at, updated_at
			) VALUES(
				?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
				'staged', 1, NULL, ?, ?
			)
		`,
			asset.ID.String(), asset.Digest.Bytes(), asset.OriginalFilename,
			asset.ManagedPath.String(), asset.Format, asset.MIMEType, asset.FileSizeBytes,
			asset.DisplayWidth, asset.DisplayHeight, asset.OrientationClass,
			boolInt(asset.HasAlpha), boolInt(asset.HasTransparency), boolInt(asset.EncodedAnimated),
			asset.EncodedFrameCount, asset.DominantColorsJSON, asset.Title, asset.Description,
			string(asset.PrimaryType), asset.Style, boolInt(asset.PixelArt), asset.AIConfidence,
			string(asset.Layout.Kind), asset.SearchTags, createdAt, createdAt,
		); err != nil {
			return fmt.Errorf("inserting staged asset: %w", err)
		}
		if err := insertSheetLayout(ctx, tx, asset); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO thumbnails(asset_id, mime_type, width, height, byte_length, data)
			VALUES(?, 'image/png', ?, ?, ?, ?)
		`,
			asset.ID.String(), asset.Thumbnail.Width, asset.Thumbnail.Height,
			len(asset.Thumbnail.Data), asset.Thumbnail.Data,
		); err != nil {
			return fmt.Errorf("inserting thumbnail: %w", err)
		}
		if err := insertTags(ctx, tx, asset); err != nil {
			return err
		}
		if err := insertAIRun(ctx, tx, asset.ItemID, asset.ID, asset.AIRun); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE import_items
			SET state = 'committing', asset_id = ?, error_code = NULL,
				error_message = NULL, updated_at = ?
			WHERE id = ? AND state = 'analyzing'
		`, asset.ID.String(), createdAt, asset.ItemID.String())
		if err != nil {
			return fmt.Errorf("linking staged asset to import item: %w", err)
		}

		return requireChanged(ctx, tx, result, workflowTableItems, asset.ItemID)
	})
}

// RecordAIRun persists a failed or otherwise non-accepted analysis attempt.
func (d *Database) RecordAIRun(
	ctx context.Context,
	itemID importer.ID,
	assetID importer.ID,
	run importer.AIRun,
) error {
	if itemID.IsZero() {
		return errors.New("recording ai run: item identifier is zero")
	}
	if err := validateAIRun(run); err != nil {
		return err
	}

	return d.withWriteTx(ctx, func(tx *sql.Tx) error {
		return insertAIRun(ctx, tx, itemID, assetID, run)
	})
}

// MarkAssetReady atomically makes a promoted asset and its import item catalog-visible.
func (d *Database) MarkAssetReady(
	ctx context.Context,
	assetID importer.ID,
	itemID importer.ID,
	readyAt time.Time,
) error {
	if assetID.IsZero() || itemID.IsZero() || readyAt.IsZero() {
		return errors.New("marking asset ready: arguments are incomplete")
	}
	return d.withWriteTx(ctx, func(tx *sql.Tx) error {
		timestamp := formatTime(readyAt)
		assetResult, err := tx.ExecContext(ctx, `
			UPDATE assets
			SET state = 'ready', imported_at = ?, updated_at = ?
			WHERE id = ? AND state = 'staged'
		`, timestamp, timestamp, assetID.String())
		if err != nil {
			return fmt.Errorf("marking asset ready: %w", err)
		}
		if err := requireChanged(ctx, tx, assetResult, workflowTableAssets, assetID); err != nil {
			return err
		}
		itemResult, err := tx.ExecContext(ctx, `
			UPDATE import_items
			SET state = 'ready', asset_id = ?, error_code = NULL,
				error_message = NULL, updated_at = ?
			WHERE id = ? AND state = 'committing' AND asset_id = ?
		`, assetID.String(), timestamp, itemID.String(), assetID.String())
		if err != nil {
			return fmt.Errorf("marking import item ready: %w", err)
		}

		return requireChanged(ctx, tx, itemResult, workflowTableItems, itemID)
	})
}

// MarkAssetIntegrityFailed hides a ready asset whose managed bytes cannot be verified.
func (d *Database) MarkAssetIntegrityFailed(
	ctx context.Context,
	assetID importer.ID,
	updatedAt time.Time,
) error {
	if assetID.IsZero() || updatedAt.IsZero() {
		return errors.New("marking asset integrity failed: arguments are incomplete")
	}
	return d.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE assets SET state = 'integrity-failed', updated_at = ?
			WHERE id = ? AND state = 'ready'
		`, formatTime(updatedAt), assetID.String())
		if err != nil {
			return fmt.Errorf("marking asset integrity failed: %w", err)
		}

		return requireChanged(ctx, tx, result, workflowTableAssets, assetID)
	})
}

func insertSheetLayout(ctx context.Context, tx *sql.Tx, asset importer.StagedAsset) error {
	if asset.Layout.Kind == catalog.LayoutKindSingle {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO asset_sheet_layouts(
			asset_id, kind, columns_count, rows_count, cell_width, cell_height,
			frame_count, animation_label, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		asset.ID.String(), string(asset.Layout.Kind), nullablePositive(asset.Layout.Columns),
		nullablePositive(asset.Layout.Rows), nullablePositive(asset.Layout.CellWidth),
		nullablePositive(asset.Layout.CellHeight), nullablePositive(asset.Layout.FrameCount),
		nullString(asset.Layout.AnimationLabel), formatTime(asset.CreatedAt),
	); err != nil {
		return fmt.Errorf("inserting asset sheet layout: %w", err)
	}

	return nil
}

func insertTags(ctx context.Context, tx *sql.Tx, asset importer.StagedAsset) error {
	seen := make(map[string]struct{}, len(asset.Tags))
	for _, tag := range asset.Tags {
		key := tag.Facet + "\x00" + tag.Slug
		if _, exists := seen[key]; exists {
			return errors.New("inserting asset tags: duplicate facet and slug")
		}
		seen[key] = struct{}{}
		var tagID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO tags(facet, slug, label) VALUES(?, ?, ?)
			ON CONFLICT(facet, slug) DO UPDATE SET label = excluded.label
			RETURNING id
		`, tag.Facet, tag.Slug, tag.Label).Scan(&tagID); err != nil {
			return fmt.Errorf("upserting asset tag: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO asset_tags(asset_id, tag_id, origin, created_at)
			VALUES(?, ?, ?, ?)
		`, asset.ID.String(), tagID, tag.Origin, formatTime(asset.CreatedAt)); err != nil {
			return fmt.Errorf("linking asset tag: %w", err)
		}
	}

	return nil
}

func insertAIRun(
	ctx context.Context,
	tx *sql.Tx,
	itemID importer.ID,
	assetID importer.ID,
	run importer.AIRun,
) error {
	var assetIDValue any
	if !assetID.IsZero() {
		assetIDValue = assetID.String()
	}
	var completedAt, latencyMS any
	if run.CompletedAt != nil {
		completedAt = formatTime(*run.CompletedAt)
		latencyMS = run.Latency.Milliseconds()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ai_runs(
			id, import_item_id, asset_id, provider, model, reasoning_effort,
			image_detail, prompt_version, schema_version, attempt_number,
			started_at, completed_at, latency_ms, request_id, usage_json,
			outcome, error_code, error_message, normalized_result_json
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		run.ID.String(), itemID.String(), assetIDValue, run.Provider, run.Model,
		run.ReasoningEffort, run.ImageDetail, run.PromptVersion, run.SchemaVersion,
		run.AttemptNumber, formatTime(run.StartedAt), completedAt, latencyMS,
		nullString(run.RequestID), nullString(run.UsageJSON), run.Outcome,
		nullString(string(run.ErrorCode)), nullString(run.ErrorMessage),
		nullString(run.NormalizedResultJSON),
	); err != nil {
		return fmt.Errorf("inserting ai run: %w", err)
	}

	return nil
}

func validateStagedAsset(asset importer.StagedAsset) error {
	if asset.ID.IsZero() || asset.ItemID.IsZero() || asset.ManagedPath == "" || asset.CreatedAt.IsZero() {
		return errors.New("committing staged asset: identity fields are incomplete")
	}
	if err := importer.ValidateOriginalFilename(asset.OriginalFilename); err != nil {
		return err
	}
	if asset.FileSizeBytes < 0 || asset.DisplayWidth < 1 || asset.DisplayHeight < 1 ||
		asset.EncodedFrameCount < 1 || asset.Thumbnail.Width < 1 || asset.Thumbnail.Width > 320 ||
		asset.Thumbnail.Height < 1 || asset.Thumbnail.Height > 320 || len(asset.Thumbnail.Data) == 0 {
		return errors.New("committing staged asset: technical metadata is invalid")
	}
	formatMIME := map[string]string{
		"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp", "gif": "image/gif",
	}
	if formatMIME[asset.Format] == "" {
		return errors.New("committing staged asset: image format is unsupported")
	}
	if asset.MIMEType != formatMIME[asset.Format] {
		return errors.New("committing staged asset: image format and mime type do not match")
	}
	switch asset.OrientationClass {
	case "square", "portrait", "landscape":
	default:
		return errors.New("committing staged asset: orientation class is unsupported")
	}
	if asset.EncodedAnimated != (asset.EncodedFrameCount > 1) {
		return errors.New("committing staged asset: animation metadata is inconsistent")
	}
	if asset.HasTransparency && !asset.HasAlpha {
		return errors.New("committing staged asset: transparency requires an alpha channel")
	}
	if !json.Valid([]byte(asset.DominantColorsJSON)) {
		return errors.New("committing staged asset: dominant colors are not valid json")
	}
	if !validRuneLength(asset.Title, 1, 160) || !validRuneLength(asset.Description, 1, 2000) ||
		!validRuneLength(asset.Style, 1, 80) || !asset.PrimaryType.Valid() || math.IsNaN(asset.AIConfidence) ||
		math.IsInf(asset.AIConfidence, 0) || asset.AIConfidence < 0 || asset.AIConfidence > 1 {
		return errors.New("committing staged asset: semantic metadata is invalid")
	}
	if err := asset.Layout.Validate(asset.DisplayWidth, asset.DisplayHeight); err != nil {
		return err
	}
	if len(asset.Tags) > 64 {
		return errors.New("committing staged asset: too many tags")
	}
	for _, tag := range asset.Tags {
		if err := importer.ValidateTag(tag); err != nil {
			return err
		}
	}
	if err := validateAIRun(asset.AIRun); err != nil {
		return err
	}
	if asset.AIRun.Outcome != "accepted" || asset.AIRun.CompletedAt == nil {
		return errors.New("committing staged asset: accepted ai run is required")
	}

	return nil
}

func validateAIRun(run importer.AIRun) error {
	if run.ID.IsZero() || run.Provider != "openai" || strings.TrimSpace(run.Model) == "" ||
		strings.TrimSpace(run.ReasoningEffort) == "" || strings.TrimSpace(run.PromptVersion) == "" ||
		strings.TrimSpace(run.SchemaVersion) == "" || run.AttemptNumber < 1 || run.StartedAt.IsZero() {
		return errors.New("recording ai run: record is incomplete")
	}
	switch run.ImageDetail {
	case "low", "high", "auto":
	default:
		return errors.New("recording ai run: image detail is unsupported")
	}
	switch run.Outcome {
	case "pending", "accepted", "retryable-error", "permanent-error", "refused",
		"invalid-response", "canceled":
	default:
		return errors.New("recording ai run: outcome is unsupported")
	}
	if run.CompletedAt != nil && run.CompletedAt.Before(run.StartedAt) {
		return errors.New("recording ai run: completion precedes start")
	}
	if err := importer.ValidateErrorFields(run.ErrorCode, run.ErrorMessage); err != nil {
		return err
	}
	if run.UsageJSON != "" && !json.Valid([]byte(run.UsageJSON)) {
		return errors.New("recording ai run: usage is not valid json")
	}
	if run.NormalizedResultJSON != "" && !json.Valid([]byte(run.NormalizedResultJSON)) {
		return errors.New("recording ai run: normalized result is not valid json")
	}

	return nil
}

func nullablePositive(value int) any {
	if value == 0 {
		return nil
	}

	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

func validRuneLength(value string, minimum, maximum int) bool {
	if !utf8.ValidString(value) {
		return false
	}
	length := utf8.RuneCountInString(value)

	return length >= minimum && length <= maximum
}
