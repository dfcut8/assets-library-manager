package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/importer"
)

// ListRecoverableItems returns nonterminal work in stable source and item order.
func (d *Database) ListRecoverableItems(
	ctx context.Context,
) (_ []importer.ItemRecord, returnErr error) {
	rows, err := d.db.QueryContext(ctx, itemSelect+`
		WHERE state NOT IN ('ready', 'duplicate')
		ORDER BY source_id, id
	`)
	if err != nil {
		return nil, fmt.Errorf("listing recoverable import items: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing recoverable item rows: %w", err))
		}
	}()

	items := make([]importer.ItemRecord, 0)
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning recoverable import item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recoverable import items: %w", err)
	}

	return items, nil
}

// SummarizeSource returns item counts used for coordinator-owned source decisions.
func (d *Database) SummarizeSource(
	ctx context.Context,
	sourceID importer.ID,
) (importer.SourceSummary, error) {
	if sourceID.IsZero() {
		return importer.SourceSummary{}, errors.New("summarizing import source: identifier is zero")
	}
	var summary importer.SourceSummary
	var exists int
	err := d.db.QueryRowContext(ctx, `
		SELECT count(s.id), count(i.id),
			coalesce(sum(i.state = 'discovered'), 0),
			coalesce(sum(i.state = 'staged'), 0),
			coalesce(sum(i.state = 'analyzing'), 0),
			coalesce(sum(i.state = 'committing'), 0),
			coalesce(sum(i.state = 'ready'), 0),
			coalesce(sum(i.state = 'duplicate'), 0),
			coalesce(sum(i.state = 'blocked'), 0),
			coalesce(sum(i.state = 'failed'), 0)
		FROM import_sources AS s
		LEFT JOIN import_items AS i ON i.source_id = s.id
		WHERE s.id = ?
	`, sourceID.String()).Scan(
		&exists, &summary.Total, &summary.Discovered, &summary.Staged, &summary.Analyzing,
		&summary.Committing, &summary.Ready, &summary.Duplicate, &summary.Blocked, &summary.Failed,
	)
	if err != nil {
		return importer.SourceSummary{}, fmt.Errorf("summarizing import source: %w", err)
	}
	if exists == 0 {
		return importer.SourceSummary{}, importer.ErrNotFound
	}
	summary.SourceID = sourceID

	return summary, nil
}

// ListRecoveryAssets returns assets whose database and filesystem state must be reconciled.
func (d *Database) ListRecoveryAssets(ctx context.Context) (_ []importer.RecoveryAsset, returnErr error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT a.id,
			(SELECT i.id FROM import_items AS i
			 WHERE i.asset_id = a.id AND i.state = 'committing' ORDER BY i.id LIMIT 1),
			a.sha256, a.file_size_bytes,
			(SELECT i.staged_path FROM import_items AS i
			 WHERE i.asset_id = a.id AND i.state = 'committing' ORDER BY i.id LIMIT 1),
			a.managed_path, a.state
		FROM assets AS a
		ORDER BY a.id
	`)
	if err != nil {
		return nil, fmt.Errorf("listing recovery assets: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing recovery asset rows: %w", err))
		}
	}()

	assets := make([]importer.RecoveryAsset, 0)
	for rows.Next() {
		var idText, managedPathText, stateText string
		var itemIDText, stagedPathText sql.NullString
		var digestBytes []byte
		var fileSize int64
		if err := rows.Scan(
			&idText, &itemIDText, &digestBytes, &fileSize,
			&stagedPathText, &managedPathText, &stateText,
		); err != nil {
			return nil, fmt.Errorf("scanning recovery asset: %w", err)
		}
		asset, err := decodeRecoveryAsset(
			idText, itemIDText, digestBytes, fileSize, stagedPathText, managedPathText, stateText,
		)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recovery assets: %w", err)
	}

	return assets, nil
}

// ListReferencedStagedPaths returns every valid staging name still owned by an import item.
func (d *Database) ListReferencedStagedPaths(
	ctx context.Context,
) (_ []importer.StagedPath, returnErr error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT DISTINCT staged_path
		FROM import_items
		WHERE staged_path IS NOT NULL AND state IN ('staged', 'analyzing', 'committing')
		ORDER BY staged_path
	`)
	if err != nil {
		return nil, fmt.Errorf("listing referenced staging paths: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing staging path rows: %w", err))
		}
	}()

	paths := make([]importer.StagedPath, 0)
	for rows.Next() {
		var pathText string
		if err := rows.Scan(&pathText); err != nil {
			return nil, fmt.Errorf("scanning referenced staging path: %w", err)
		}
		stagedPath, err := importer.ParseStagedPath(pathText)
		if err != nil {
			return nil, fmt.Errorf("decoding referenced staging path: %w", err)
		}
		paths = append(paths, stagedPath)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating referenced staging paths: %w", err)
	}

	return paths, nil
}

// ListPendingDeletions returns sources whose previously eligible deletion may be retried at startup.
func (d *Database) ListPendingDeletions(
	ctx context.Context,
) (_ []importer.PendingDeletion, returnErr error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, source_path, discovery_fingerprint, deletion_state
		FROM import_sources
		WHERE deletion_state IN ('eligible', 'pending', 'failed')
		ORDER BY source_path
	`)
	if err != nil {
		return nil, fmt.Errorf("listing pending source deletions: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing pending deletion rows: %w", err))
		}
	}()

	deletions := make([]importer.PendingDeletion, 0)
	for rows.Next() {
		var idText, sourcePathText, fingerprintText, stateText string
		if err := rows.Scan(&idText, &sourcePathText, &fingerprintText, &stateText); err != nil {
			return nil, fmt.Errorf("scanning pending source deletion: %w", err)
		}
		deletion, err := decodePendingDeletion(idText, sourcePathText, fingerprintText, stateText)
		if err != nil {
			return nil, err
		}
		deletions = append(deletions, deletion)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pending source deletions: %w", err)
	}

	return deletions, nil
}

// FailStagedAsset records loss of both staged and managed copies while preserving its source.
func (d *Database) FailStagedAsset(
	ctx context.Context,
	assetID importer.ID,
	itemID importer.ID,
	code importer.ErrorCode,
	message string,
	failedAt time.Time,
) error {
	if assetID.IsZero() || itemID.IsZero() || failedAt.IsZero() {
		return errors.New("failing staged asset: arguments are incomplete")
	}
	if err := importer.ValidateErrorFields(code, message); err != nil {
		return err
	}
	return d.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE import_items
			SET state = 'failed', asset_id = NULL, error_code = ?, error_message = ?, updated_at = ?
			WHERE id = ? AND asset_id = ? AND state = 'committing'
		`, nullString(string(code)), nullString(message), formatTime(failedAt),
			itemID.String(), assetID.String())
		if err != nil {
			return fmt.Errorf("failing staged import item: %w", err)
		}
		if err := requireChanged(ctx, tx, result, workflowTableItems, itemID); err != nil {
			return err
		}
		assetResult, err := tx.ExecContext(
			ctx, `DELETE FROM assets WHERE id = ? AND state = 'staged'`, assetID.String(),
		)
		if err != nil {
			return fmt.Errorf("removing lost staged asset: %w", err)
		}
		if err := requireChanged(ctx, tx, assetResult, workflowTableAssets, assetID); err != nil {
			return err
		}

		return nil
	})
}

func decodeRecoveryAsset(
	idText string,
	itemIDText sql.NullString,
	digestBytes []byte,
	fileSize int64,
	stagedPathText sql.NullString,
	managedPathText string,
	stateText string,
) (importer.RecoveryAsset, error) {
	id, err := importer.ParseID(idText)
	if err != nil {
		return importer.RecoveryAsset{}, fmt.Errorf("decoding recovery asset identifier: %w", err)
	}
	var itemID importer.ID
	if itemIDText.Valid {
		itemID, err = importer.ParseID(itemIDText.String)
		if err != nil {
			return importer.RecoveryAsset{}, fmt.Errorf("decoding recovery item identifier: %w", err)
		}
	}
	digest, err := digestFromBytes(digestBytes)
	if err != nil {
		return importer.RecoveryAsset{}, err
	}
	var stagedPath importer.StagedPath
	if stagedPathText.Valid {
		stagedPath, err = importer.ParseStagedPath(stagedPathText.String)
		if err != nil {
			return importer.RecoveryAsset{}, fmt.Errorf("decoding recovery staging path: %w", err)
		}
	}
	managedPath, err := importer.NewManagedPath(managedPathText)
	if err != nil {
		return importer.RecoveryAsset{}, fmt.Errorf("decoding recovery managed path: %w", err)
	}
	state := importer.AssetState(stateText)
	if !state.Valid() {
		return importer.RecoveryAsset{}, errors.New("decoding recovery asset: invalid asset state")
	}
	if state == importer.AssetStateStaged && (itemID.IsZero() || stagedPath == "") {
		return importer.RecoveryAsset{}, errors.New("decoding recovery asset: staged asset has no committing item")
	}

	return importer.RecoveryAsset{
		ID: id, ItemID: itemID, Digest: digest, Size: fileSize, StagedPath: stagedPath,
		ManagedPath: managedPath, State: state,
	}, nil
}

func decodePendingDeletion(
	idText, sourcePathText, fingerprintText, stateText string,
) (importer.PendingDeletion, error) {
	id, err := importer.ParseID(idText)
	if err != nil {
		return importer.PendingDeletion{}, fmt.Errorf("decoding pending deletion identifier: %w", err)
	}
	sourcePath, err := importer.NewSourcePath(sourcePathText)
	if err != nil {
		return importer.PendingDeletion{}, fmt.Errorf("decoding pending deletion path: %w", err)
	}
	fingerprint, err := importer.ParseDigest(fingerprintText)
	if err != nil {
		return importer.PendingDeletion{}, fmt.Errorf("decoding pending deletion fingerprint: %w", err)
	}
	state := importer.DeletionState(stateText)
	if !state.Valid() {
		return importer.PendingDeletion{}, errors.New("decoding pending deletion: invalid deletion state")
	}

	return importer.PendingDeletion{ID: id, Path: sourcePath, Fingerprint: fingerprint, State: state}, nil
}
