package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/importer"
)

type rowScanner interface {
	Scan(dest ...any) error
}

type workflowTable uint8

const (
	workflowTableAssets workflowTable = iota + 1
	workflowTableItems
	workflowTableSources
)

// CreateImportSource inserts a newly discovered source or returns the matching persisted identity.
func (d *Database) CreateImportSource(
	ctx context.Context,
	input importer.SourceRecord,
) (importer.SourceRecord, error) {
	if err := validateSourceRecord(input); err != nil {
		return importer.SourceRecord{}, err
	}
	var record importer.SourceRecord
	err := d.withWriteTx(ctx, func(tx *sql.Tx) error {
		existing, err := querySourceByIdentity(
			ctx, tx, input.Path, input.DiscoveryFingerprint,
		)
		if err == nil {
			record = existing

			return nil
		}
		if !errors.Is(err, importer.ErrNotFound) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO import_sources(
				id, source_path, source_type, discovery_fingerprint, state,
				deletion_state, retained_reason, error_code, error_message,
				discovered_at, updated_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			input.ID.String(), input.Path.String(), string(input.Type),
			input.DiscoveryFingerprint.String(), string(input.State), string(input.DeletionState),
			nullString(input.RetainedReason), nullString(string(input.ErrorCode)),
			nullString(input.ErrorMessage), formatTime(input.DiscoveredAt), formatTime(input.UpdatedAt),
		); err != nil {
			return fmt.Errorf("inserting import source: %w", err)
		}
		record = input

		return nil
	})

	return record, err
}

// FindImportSource returns the source record for an incoming path and fingerprint.
func (d *Database) FindImportSource(
	ctx context.Context,
	path importer.SourcePath,
	fingerprint importer.Digest,
) (importer.SourceRecord, error) {
	return querySourceByIdentity(ctx, d.db, path, fingerprint)
}

// CreateImportItem inserts a discovered image item or returns its existing durable record.
func (d *Database) CreateImportItem(
	ctx context.Context,
	input importer.ItemRecord,
) (record importer.ItemRecord, returnErr error) {
	if err := validateItemRecord(input); err != nil {
		return importer.ItemRecord{}, err
	}
	returnErr = d.withWriteTx(ctx, func(tx *sql.Tx) error {
		existing, err := queryItemBySourceEntry(ctx, tx, input.SourceID, input.ZIPEntryName)
		if err == nil {
			record = existing

			return nil
		}
		if !errors.Is(err, importer.ErrNotFound) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO import_items(
				id, source_id, zip_entry_name, staged_path, sha256, asset_id,
				state, attempt_count, error_code, error_message, created_at, updated_at
			) VALUES(?, ?, ?, NULL, NULL, NULL, ?, 0, NULL, NULL, ?, ?)
		`,
			input.ID.String(), input.SourceID.String(), nullString(input.ZIPEntryName),
			string(input.State), formatTime(input.CreatedAt), formatTime(input.UpdatedAt),
		); err != nil {
			return fmt.Errorf("inserting import item: %w", err)
		}
		record = input

		return nil
	})

	return record, returnErr
}

// FindImportItem returns an item by its durable identifier.
func (d *Database) FindImportItem(ctx context.Context, id importer.ID) (importer.ItemRecord, error) {
	if id.IsZero() {
		return importer.ItemRecord{}, errors.New("finding import item: identifier is zero")
	}

	record, err := scanItem(d.db.QueryRowContext(ctx, itemSelect+" WHERE id = ?", id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return importer.ItemRecord{}, importer.ErrNotFound
	}
	if err != nil {
		return importer.ItemRecord{}, fmt.Errorf("finding import item: %w", err)
	}

	return record, nil
}

// TransitionImportItem applies one conditional workflow state change.
func (d *Database) TransitionImportItem(ctx context.Context, transition importer.ItemTransition) error {
	if err := validateItemTransition(transition); err != nil {
		return err
	}
	return d.withWriteTx(ctx, func(tx *sql.Tx) error {
		var stagedPath any
		if transition.StagedPath != "" {
			stagedPath = transition.StagedPath.String()
		}
		var digest any
		if transition.Digest != (importer.Digest{}) {
			digest = transition.Digest.Bytes()
		}
		var assetID any
		if !transition.AssetID.IsZero() {
			assetID = transition.AssetID.String()
		}
		attemptIncrement := 0
		if transition.To == importer.ItemStateAnalyzing {
			attemptIncrement = 1
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE import_items
			SET state = ?,
				staged_path = COALESCE(?, staged_path),
				sha256 = COALESCE(?, sha256),
				asset_id = COALESCE(?, asset_id),
				attempt_count = attempt_count + ?,
				error_code = ?,
				error_message = ?,
				updated_at = ?
			WHERE id = ? AND state = ?
		`,
			string(transition.To), stagedPath, digest, assetID, attemptIncrement,
			nullString(string(transition.ErrorCode)), nullString(transition.ErrorMessage),
			formatTime(transition.UpdatedAt), transition.ID.String(), string(transition.From),
		)
		if err != nil {
			return fmt.Errorf("transitioning import item: %w", err)
		}

		return requireChanged(ctx, tx, result, workflowTableItems, transition.ID)
	})
}

// TransitionImportSource applies one conditional aggregate source state change.
func (d *Database) TransitionImportSource(ctx context.Context, transition importer.SourceTransition) error {
	if err := validateSourceTransition(transition); err != nil {
		return err
	}
	return d.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE import_sources
			SET state = ?, deletion_state = ?, retained_reason = ?,
				error_code = ?, error_message = ?, updated_at = ?
			WHERE id = ? AND state = ?
		`,
			string(transition.To), string(transition.DeletionState),
			nullString(transition.RetainedReason), nullString(string(transition.ErrorCode)),
			nullString(transition.ErrorMessage), formatTime(transition.UpdatedAt),
			transition.ID.String(), string(transition.From),
		)
		if err != nil {
			return fmt.Errorf("transitioning import source: %w", err)
		}

		return requireChanged(ctx, tx, result, workflowTableSources, transition.ID)
	})
}

// FindReadyByDigest returns the ready asset that owns a full digest.
func (d *Database) FindReadyByDigest(
	ctx context.Context,
	digest importer.Digest,
) (importer.AssetRef, error) {
	var idText, managedPathText string
	var digestBytes []byte
	err := d.db.QueryRowContext(ctx, `
		SELECT id, sha256, managed_path
		FROM assets
		WHERE sha256 = ? AND state = 'ready'
	`, digest.Bytes()).Scan(&idText, &digestBytes, &managedPathText)
	if errors.Is(err, sql.ErrNoRows) {
		return importer.AssetRef{}, importer.ErrNotFound
	}
	if err != nil {
		return importer.AssetRef{}, fmt.Errorf("finding ready asset by digest: %w", err)
	}
	id, err := importer.ParseID(idText)
	if err != nil {
		return importer.AssetRef{}, fmt.Errorf("decoding ready asset identifier: %w", err)
	}
	storedDigest, err := digestFromBytes(digestBytes)
	if err != nil {
		return importer.AssetRef{}, err
	}
	managedPath, err := importer.NewManagedPath(managedPathText)
	if err != nil {
		return importer.AssetRef{}, fmt.Errorf("decoding ready managed path: %w", err)
	}

	return importer.AssetRef{ID: id, Digest: storedDigest, ManagedPath: managedPath}, nil
}

func (d *Database) withWriteTx(ctx context.Context, fn func(*sql.Tx) error) (returnErr error) {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.writeGate:
	}
	defer func() { d.writeGate <- struct{}{} }()

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting sqlite write transaction: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("rolling back sqlite write transaction: %w", err))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing sqlite write transaction: %w", err)
	}

	return nil
}

const sourceSelect = `
	SELECT id, source_path, source_type, discovery_fingerprint, state,
		deletion_state, retained_reason, error_code, error_message,
		discovered_at, updated_at
	FROM import_sources`

func querySourceByIdentity(
	ctx context.Context,
	queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	path importer.SourcePath,
	fingerprint importer.Digest,
) (importer.SourceRecord, error) {
	record, err := scanSource(queryer.QueryRowContext(
		ctx,
		sourceSelect+" WHERE source_path = ? AND discovery_fingerprint = ?",
		path.String(),
		fingerprint.String(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return importer.SourceRecord{}, importer.ErrNotFound
	}
	if err != nil {
		return importer.SourceRecord{}, fmt.Errorf("querying import source: %w", err)
	}

	return record, nil
}

func scanSource(scanner rowScanner) (importer.SourceRecord, error) {
	var (
		idText, sourcePathText, sourceTypeText, fingerprintText string
		stateText, deletionStateText                            string
		retainedReason, errorCode, errorMessage                 sql.NullString
		discoveredAtText, updatedAtText                         string
	)
	if err := scanner.Scan(
		&idText, &sourcePathText, &sourceTypeText, &fingerprintText, &stateText,
		&deletionStateText, &retainedReason, &errorCode, &errorMessage,
		&discoveredAtText, &updatedAtText,
	); err != nil {
		return importer.SourceRecord{}, err
	}
	id, err := importer.ParseID(idText)
	if err != nil {
		return importer.SourceRecord{}, err
	}
	sourcePath, err := importer.NewSourcePath(sourcePathText)
	if err != nil {
		return importer.SourceRecord{}, err
	}
	fingerprint, err := importer.ParseDigest(fingerprintText)
	if err != nil {
		return importer.SourceRecord{}, err
	}
	discoveredAt, err := parseTime(discoveredAtText)
	if err != nil {
		return importer.SourceRecord{}, err
	}
	updatedAt, err := parseTime(updatedAtText)
	if err != nil {
		return importer.SourceRecord{}, err
	}

	return importer.SourceRecord{
		ID:                   id,
		Path:                 sourcePath,
		Type:                 importer.SourceType(sourceTypeText),
		DiscoveryFingerprint: fingerprint,
		State:                importer.SourceState(stateText),
		DeletionState:        importer.DeletionState(deletionStateText),
		RetainedReason:       retainedReason.String,
		ErrorCode:            importer.ErrorCode(errorCode.String),
		ErrorMessage:         errorMessage.String,
		DiscoveredAt:         discoveredAt,
		UpdatedAt:            updatedAt,
	}, nil
}

const itemSelect = `
	SELECT id, source_id, zip_entry_name, staged_path, sha256, asset_id,
		state, attempt_count, error_code, error_message, created_at, updated_at
	FROM import_items`

func queryItemBySourceEntry(
	ctx context.Context,
	queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	sourceID importer.ID,
	entryName string,
) (importer.ItemRecord, error) {
	record, err := scanItem(queryer.QueryRowContext(
		ctx,
		itemSelect+" WHERE source_id = ? AND ifnull(zip_entry_name, '') = ?",
		sourceID.String(), entryName,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return importer.ItemRecord{}, importer.ErrNotFound
	}
	if err != nil {
		return importer.ItemRecord{}, fmt.Errorf("querying import item: %w", err)
	}

	return record, nil
}

func scanItem(scanner rowScanner) (importer.ItemRecord, error) {
	var (
		idText, sourceIDText, stateText, createdAtText, updatedAtText string
		entryName, stagedPathText, assetIDText                        sql.NullString
		digestBytes                                                   []byte
		attemptCount                                                  int
		errorCode, errorMessage                                       sql.NullString
	)
	if err := scanner.Scan(
		&idText, &sourceIDText, &entryName, &stagedPathText, &digestBytes, &assetIDText,
		&stateText, &attemptCount, &errorCode, &errorMessage, &createdAtText, &updatedAtText,
	); err != nil {
		return importer.ItemRecord{}, err
	}
	id, err := importer.ParseID(idText)
	if err != nil {
		return importer.ItemRecord{}, err
	}
	sourceID, err := importer.ParseID(sourceIDText)
	if err != nil {
		return importer.ItemRecord{}, err
	}
	var stagedPath importer.StagedPath
	if stagedPathText.Valid {
		stagedPath, err = importer.ParseStagedPath(stagedPathText.String)
		if err != nil {
			return importer.ItemRecord{}, err
		}
	}
	var digest importer.Digest
	if len(digestBytes) != 0 {
		digest, err = digestFromBytes(digestBytes)
		if err != nil {
			return importer.ItemRecord{}, err
		}
	}
	var assetID importer.ID
	if assetIDText.Valid {
		assetID, err = importer.ParseID(assetIDText.String)
		if err != nil {
			return importer.ItemRecord{}, err
		}
	}
	createdAt, err := parseTime(createdAtText)
	if err != nil {
		return importer.ItemRecord{}, err
	}
	updatedAt, err := parseTime(updatedAtText)
	if err != nil {
		return importer.ItemRecord{}, err
	}

	return importer.ItemRecord{
		ID:           id,
		SourceID:     sourceID,
		ZIPEntryName: entryName.String,
		StagedPath:   stagedPath,
		Digest:       digest,
		AssetID:      assetID,
		State:        importer.ItemState(stateText),
		AttemptCount: attemptCount,
		ErrorCode:    importer.ErrorCode(errorCode.String),
		ErrorMessage: errorMessage.String,
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
	}, nil
}

func validateSourceRecord(record importer.SourceRecord) error {
	if record.ID.IsZero() || record.Path == "" || !record.Type.Valid() || !record.State.Valid() ||
		!record.DeletionState.Valid() || record.DiscoveredAt.IsZero() || record.UpdatedAt.IsZero() {
		return errors.New("creating import source: record is incomplete")
	}
	if err := importer.ValidateErrorFields(record.ErrorCode, record.ErrorMessage); err != nil {
		return err
	}

	return nil
}

func validateItemRecord(record importer.ItemRecord) error {
	if record.ID.IsZero() || record.SourceID.IsZero() || record.State != importer.ItemStateDiscovered ||
		record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return errors.New("creating import item: record is incomplete")
	}
	if len(record.ZIPEntryName) > 1024 || strings.ContainsRune(record.ZIPEntryName, '\x00') {
		return errors.New("creating import item: zip entry name is invalid")
	}

	return nil
}

func validateItemTransition(transition importer.ItemTransition) error {
	if transition.ID.IsZero() || !transition.From.Valid() || !transition.To.Valid() ||
		!transition.From.CanTransitionTo(transition.To) || transition.UpdatedAt.IsZero() {
		return importer.ErrInvalidTransition
	}
	if transition.To == importer.ItemStateStaged && transition.StagedPath == "" {
		return errors.New("transitioning import item: staged state requires a staging path")
	}
	if transition.To == importer.ItemStateStaged && transition.Digest == (importer.Digest{}) {
		return errors.New("transitioning import item: staged state requires a digest")
	}
	if (transition.To == importer.ItemStateCommitting || transition.To == importer.ItemStateReady ||
		transition.To == importer.ItemStateDuplicate) && transition.AssetID.IsZero() {
		return errors.New("transitioning import item: target state requires an asset identifier")
	}
	if err := importer.ValidateErrorFields(transition.ErrorCode, transition.ErrorMessage); err != nil {
		return err
	}

	return nil
}

func validateSourceTransition(transition importer.SourceTransition) error {
	if transition.ID.IsZero() || !transition.From.Valid() || !transition.To.Valid() ||
		!transition.DeletionState.Valid() || transition.UpdatedAt.IsZero() {
		return errors.New("transitioning import source: transition is incomplete")
	}
	if err := importer.ValidateErrorFields(transition.ErrorCode, transition.ErrorMessage); err != nil {
		return err
	}

	return nil
}

func requireChanged(
	ctx context.Context,
	tx *sql.Tx,
	result sql.Result,
	table workflowTable,
	id importer.ID,
) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking workflow transition: %w", err)
	}
	if changed == 1 {
		return nil
	}
	var exists int
	var query string
	switch table {
	case workflowTableAssets:
		query = "SELECT count(*) FROM assets WHERE id = ?"
	case workflowTableItems:
		query = "SELECT count(*) FROM import_items WHERE id = ?"
	case workflowTableSources:
		query = "SELECT count(*) FROM import_sources WHERE id = ?"
	default:
		return errors.New("checking workflow record: invalid table")
	}
	if err := tx.QueryRowContext(ctx, query, id.String()).Scan(&exists); err != nil {
		return fmt.Errorf("checking workflow record: %w", err)
	}
	if exists == 0 {
		return importer.ErrNotFound
	}

	return importer.ErrInvalidTransition
}

func digestFromBytes(data []byte) (importer.Digest, error) {
	if len(data) != 32 {
		return importer.Digest{}, fmt.Errorf("decoding sqlite digest: got %d bytes", len(data))
	}
	var digest importer.Digest
	copy(digest[:], data)

	return digest, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding sqlite timestamp: %w", err)
	}

	return parsed, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}

	return value
}
