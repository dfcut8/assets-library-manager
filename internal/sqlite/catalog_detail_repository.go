package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
)

// Get loads one ready catalog asset with layout, tags, and latest AI provenance.
func (d *Database) Get(ctx context.Context, id catalog.AssetID) (catalog.AssetDetail, error) {
	if _, err := catalog.ParseAssetID(id.String()); err != nil {
		return catalog.AssetDetail{}, err
	}
	detail, err := scanAssetDetail(d.db.QueryRowContext(ctx, `
		SELECT a.id, a.sha256, a.original_filename, a.managed_path, a.format,
			a.mime_type, a.file_size_bytes, a.display_width, a.display_height,
			a.orientation_class, a.has_alpha, a.has_transparency,
			a.encoded_animated, a.encoded_frame_count, a.dominant_colors_json,
			coalesce(a.title, ''), coalesce(a.description, ''),
			coalesce(a.primary_type, ''), coalesce(a.style, ''),
			coalesce(a.pixel_art, 0), coalesce(a.ai_confidence, 0),
			a.layout_kind, a.version, a.imported_at, a.updated_at,
			asl.columns_count, asl.rows_count, asl.cell_width, asl.cell_height,
			asl.frame_count, asl.animation_label,
			th.mime_type, th.width, th.height, th.data,
			ar.provider, ar.model, ar.reasoning_effort, ar.image_detail,
			ar.prompt_version, ar.schema_version, ar.attempt_number, ar.outcome,
			ar.latency_ms, ar.request_id, ar.started_at, ar.completed_at
		FROM assets AS a
		JOIN thumbnails AS th ON th.asset_id = a.id
		LEFT JOIN asset_sheet_layouts AS asl ON asl.asset_id = a.id
		LEFT JOIN ai_runs AS ar ON ar.id = (
			SELECT latest.id FROM ai_runs AS latest
			WHERE latest.asset_id = a.id
			ORDER BY latest.attempt_number DESC, latest.started_at DESC, latest.id DESC
			LIMIT 1
		)
		WHERE a.id = ? AND a.state = 'ready'
	`, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.AssetDetail{}, catalog.ErrNotFound
	}
	if err != nil {
		return catalog.AssetDetail{}, fmt.Errorf("loading catalog asset detail: %w", err)
	}
	summaries := []catalog.AssetSummary{detail.AssetSummary}
	if err := d.loadSummaryTags(ctx, summaries); err != nil {
		return catalog.AssetDetail{}, err
	}
	detail.Tags = summaries[0].Tags

	return detail, nil
}

// GetThumbnail loads bounded PNG bytes only for a ready asset.
func (d *Database) GetThumbnail(ctx context.Context, id catalog.AssetID) (catalog.Thumbnail, error) {
	if _, err := catalog.ParseAssetID(id.String()); err != nil {
		return catalog.Thumbnail{}, err
	}
	var thumbnail catalog.Thumbnail
	var updatedAtText string
	err := d.db.QueryRowContext(ctx, `
		SELECT t.mime_type, t.width, t.height, t.data, a.version, a.updated_at
		FROM thumbnails AS t
		JOIN assets AS a ON a.id = t.asset_id
		WHERE a.id = ? AND a.state = 'ready'
	`, id.String()).Scan(
		&thumbnail.MIMEType, &thumbnail.Width, &thumbnail.Height,
		&thumbnail.Data, &thumbnail.Version, &updatedAtText,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.Thumbnail{}, catalog.ErrNotFound
	}
	if err != nil {
		return catalog.Thumbnail{}, fmt.Errorf("loading catalog thumbnail: %w", err)
	}
	thumbnail.UpdatedAt, err = parseTime(updatedAtText)
	if err != nil {
		return catalog.Thumbnail{}, err
	}
	thumbnail.Data = append([]byte(nil), thumbnail.Data...)

	return thumbnail, nil
}

// GetOriginal resolves only a ready asset's managed original metadata.
func (d *Database) GetOriginal(ctx context.Context, id catalog.AssetID) (catalog.Original, error) {
	if _, err := catalog.ParseAssetID(id.String()); err != nil {
		return catalog.Original{}, err
	}
	var original catalog.Original
	var idText string
	var digest []byte
	var updatedAtText string
	err := d.db.QueryRowContext(ctx, `
		SELECT id, managed_path, original_filename, mime_type, file_size_bytes,
			sha256, updated_at
		FROM assets
		WHERE id = ? AND state = 'ready'
	`, id.String()).Scan(
		&idText, &original.ManagedPath, &original.OriginalFilename,
		&original.MIMEType, &original.FileSizeBytes, &digest, &updatedAtText,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.Original{}, catalog.ErrNotFound
	}
	if err != nil {
		return catalog.Original{}, fmt.Errorf("loading catalog original: %w", err)
	}
	original.ID, err = catalog.ParseAssetID(idText)
	if err != nil {
		return catalog.Original{}, err
	}
	if len(digest) != 32 {
		return catalog.Original{}, errors.New("loading catalog original: invalid digest")
	}
	original.SHA256 = hex.EncodeToString(digest)
	original.UpdatedAt, err = parseTime(updatedAtText)
	if err != nil {
		return catalog.Original{}, err
	}

	return original, nil
}

type detailScanner interface {
	Scan(...any) error
}

func scanAssetDetail(scanner detailScanner) (catalog.AssetDetail, error) {
	var detail catalog.AssetDetail
	var idText, primaryTypeText, layoutKindText, importedAtText, updatedAtText string
	var digest []byte
	var dominantColorsJSON string
	var columns, rows, cellWidth, cellHeight, frameCount sql.NullInt64
	var animationLabel sql.NullString
	var provider, model, effort, imageDetail, promptVersion, schemaVersion sql.NullString
	var attemptNumber, latencyMS sql.NullInt64
	var outcome, requestID, startedAtText, completedAtText sql.NullString
	if err := scanner.Scan(
		&idText, &digest, &detail.OriginalFilename, &detail.ManagedPath, &detail.Format,
		&detail.MIMEType, &detail.FileSizeBytes, &detail.DisplayWidth, &detail.DisplayHeight,
		&detail.OrientationClass, &detail.HasAlpha, &detail.HasTransparency,
		&detail.EncodedAnimated, &detail.EncodedFrameCount, &dominantColorsJSON,
		&detail.Title, &detail.Description, &primaryTypeText, &detail.Style,
		&detail.PixelArt, &detail.AIConfidence, &layoutKindText, &detail.Version,
		&importedAtText, &updatedAtText,
		&columns, &rows, &cellWidth, &cellHeight, &frameCount, &animationLabel,
		&detail.Thumbnail.MIMEType, &detail.Thumbnail.Width,
		&detail.Thumbnail.Height, &detail.Thumbnail.Data,
		&provider, &model, &effort, &imageDetail, &promptVersion, &schemaVersion,
		&attemptNumber, &outcome, &latencyMS, &requestID, &startedAtText, &completedAtText,
	); err != nil {
		return catalog.AssetDetail{}, err
	}
	id, err := catalog.ParseAssetID(idText)
	if err != nil {
		return catalog.AssetDetail{}, err
	}
	if len(digest) != 32 {
		return catalog.AssetDetail{}, errors.New("decoding catalog detail: invalid digest")
	}
	detail.ID = id
	detail.SHA256 = hex.EncodeToString(digest)
	detail.PrimaryType = catalog.PrimaryType(primaryTypeText)
	if !detail.PrimaryType.Valid() {
		return catalog.AssetDetail{}, errors.New("decoding catalog detail: invalid primary type")
	}
	detail.ImportedAt, err = parseTime(importedAtText)
	if err != nil {
		return catalog.AssetDetail{}, err
	}
	detail.UpdatedAt, err = parseTime(updatedAtText)
	if err != nil {
		return catalog.AssetDetail{}, err
	}
	detail.Thumbnail.Version = detail.Version
	detail.Thumbnail.UpdatedAt = detail.UpdatedAt
	detail.Thumbnail.Data = append([]byte(nil), detail.Thumbnail.Data...)
	if err := json.Unmarshal([]byte(dominantColorsJSON), &detail.DominantColors); err != nil {
		return catalog.AssetDetail{}, fmt.Errorf("decoding dominant colors: %w", err)
	}
	detail.Layout = catalog.Layout{
		Kind: catalog.LayoutKind(layoutKindText), Columns: int(columns.Int64), Rows: int(rows.Int64),
		CellWidth: int(cellWidth.Int64), CellHeight: int(cellHeight.Int64),
		FrameCount: int(frameCount.Int64), AnimationLabel: animationLabel.String,
	}
	if err := detail.Layout.Validate(detail.DisplayWidth, detail.DisplayHeight); err != nil {
		return catalog.AssetDetail{}, fmt.Errorf("decoding catalog layout: %w", err)
	}
	if provider.Valid {
		provenance := &catalog.AIProvenance{
			Provider: provider.String, Model: model.String, ReasoningEffort: effort.String,
			ImageDetail: imageDetail.String, PromptVersion: promptVersion.String,
			SchemaVersion: schemaVersion.String, AttemptNumber: int(attemptNumber.Int64),
			Outcome: outcome.String, Latency: time.Duration(latencyMS.Int64) * time.Millisecond,
			RequestID: requestID.String,
		}
		provenance.StartedAt, err = parseTime(startedAtText.String)
		if err != nil {
			return catalog.AssetDetail{}, err
		}
		if completedAtText.Valid {
			completedAt, err := parseTime(completedAtText.String)
			if err != nil {
				return catalog.AssetDetail{}, err
			}
			provenance.CompletedAt = &completedAt
		}
		detail.AI = provenance
	}

	return detail, nil
}
