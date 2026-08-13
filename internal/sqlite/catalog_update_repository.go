package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
)

// UpdateSemanticMetadata replaces editable metadata and increments the optimistic version.
func (d *Database) UpdateSemanticMetadata(
	ctx context.Context,
	id catalog.AssetID,
	edit catalog.MetadataEdit,
) (newVersion int, returnErr error) {
	if _, err := catalog.ParseAssetID(id.String()); err != nil {
		return 0, err
	}
	edit, err := catalog.NormalizeMetadataEdit(edit)
	if err != nil {
		return 0, err
	}
	returnErr = d.withWriteTx(ctx, func(tx *sql.Tx) error {
		var width, height, currentVersion int
		err := tx.QueryRowContext(ctx, `
			SELECT display_width, display_height, version
			FROM assets
			WHERE id = ? AND state = 'ready'
		`, id.String()).Scan(&width, &height, &currentVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return catalog.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("loading asset for metadata update: %w", err)
		}
		if currentVersion != edit.Version {
			return &catalog.StaleEditError{
				SubmittedVersion: edit.Version,
				CurrentVersion:   currentVersion,
			}
		}
		if err := edit.Layout.Validate(width, height); err != nil {
			return &catalog.ValidationError{Fields: map[string]string{"layout": err.Error()}}
		}
		if _, err := tx.ExecContext(
			ctx, "DELETE FROM asset_sheet_layouts WHERE asset_id = ?", id.String(),
		); err != nil {
			return fmt.Errorf("removing previous asset layout: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx, "DELETE FROM asset_tags WHERE asset_id = ?", id.String(),
		); err != nil {
			return fmt.Errorf("removing previous asset tags: %w", err)
		}
		updatedAt := time.Now().UTC()
		result, err := tx.ExecContext(ctx, `
			UPDATE assets
			SET title = ?, description = ?, primary_type = ?, style = ?,
				pixel_art = ?, layout_kind = ?, search_tags = ?,
				version = version + 1, updated_at = ?
			WHERE id = ? AND state = 'ready' AND version = ?
		`,
			edit.Title, edit.Description, string(edit.PrimaryType), edit.Style,
			boolInt(edit.PixelArt), string(edit.Layout.Kind), catalog.FlattenTags(edit.Tags),
			formatTime(updatedAt), id.String(), edit.Version,
		)
		if err != nil {
			return fmt.Errorf("updating semantic metadata: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking semantic metadata update: %w", err)
		}
		if changed != 1 {
			return &catalog.StaleEditError{
				SubmittedVersion: edit.Version,
				CurrentVersion:   currentVersion,
			}
		}
		if err := replaceCatalogTags(ctx, tx, id, edit.Tags, updatedAt); err != nil {
			return err
		}
		if err := replaceCatalogLayout(ctx, tx, id, edit.Layout, updatedAt); err != nil {
			return err
		}
		newVersion = edit.Version + 1

		return nil
	})
	if returnErr != nil {
		return 0, returnErr
	}

	return newVersion, nil
}

func replaceCatalogTags(
	ctx context.Context,
	tx *sql.Tx,
	assetID catalog.AssetID,
	tags []catalog.Tag,
	updatedAt time.Time,
) error {
	for _, tag := range tags {
		var tagID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO tags(facet, slug, label) VALUES(?, ?, ?)
			ON CONFLICT(facet, slug) DO UPDATE SET label = excluded.label
			RETURNING id
		`, tag.Facet, tag.Slug, tag.Label).Scan(&tagID); err != nil {
			return fmt.Errorf("upserting edited tag: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO asset_tags(asset_id, tag_id, origin, created_at)
			VALUES(?, ?, 'user', ?)
		`, assetID.String(), tagID, formatTime(updatedAt)); err != nil {
			return fmt.Errorf("linking edited tag: %w", err)
		}
	}

	return nil
}

func replaceCatalogLayout(
	ctx context.Context,
	tx *sql.Tx,
	assetID catalog.AssetID,
	layout catalog.Layout,
	updatedAt time.Time,
) error {
	if layout.Kind == catalog.LayoutKindSingle {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO asset_sheet_layouts(
			asset_id, kind, columns_count, rows_count, cell_width, cell_height,
			frame_count, animation_label, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		assetID.String(), string(layout.Kind), nullablePositive(layout.Columns),
		nullablePositive(layout.Rows), nullablePositive(layout.CellWidth),
		nullablePositive(layout.CellHeight), nullablePositive(layout.FrameCount),
		nullString(layout.AnimationLabel), formatTime(updatedAt),
	); err != nil {
		return fmt.Errorf("inserting edited sheet layout: %w", err)
	}

	return nil
}
