package importer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

const orphanStagingAge = 24 * time.Hour

// Recover reconciles persisted workflow state with managed and staging files before HTTP startup.
func (coordinator *Coordinator) Recover(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := coordinator.recoverAssets(ctx); err != nil {
		return err
	}
	if err := coordinator.resetRecoverableItems(ctx); err != nil {
		return err
	}
	referenced, err := coordinator.repository.ListReferencedStagedPaths(ctx)
	if err != nil {
		return fmt.Errorf("listing referenced staging files: %w", err)
	}
	if _, err := coordinator.store.CleanOrphanStaging(
		ctx, referenced, coordinator.now().UTC().Add(-orphanStagingAge),
	); err != nil {
		return fmt.Errorf("cleaning orphan staging files: %w", err)
	}
	if err := coordinator.recoverSourceDeletions(ctx); err != nil {
		return err
	}

	return nil
}

func (coordinator *Coordinator) recoverAssets(ctx context.Context) error {
	assets, err := coordinator.repository.ListRecoveryAssets(ctx)
	if err != nil {
		return fmt.Errorf("listing recovery assets: %w", err)
	}
	for _, asset := range assets {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch asset.State {
		case AssetStateReady:
			matches, verifyErr := coordinator.store.VerifyManaged(
				ctx, asset.ManagedPath, asset.Digest, asset.Size,
			)
			if verifyErr == nil && matches {
				continue
			}
			if err := coordinator.repository.MarkAssetIntegrityFailed(
				ctx, asset.ID, coordinator.now().UTC(),
			); err != nil {
				return fmt.Errorf("marking ready asset integrity-failed: %w", err)
			}
			coordinator.logger.Error(
				"managed asset failed recovery verification",
				"asset_id", asset.ID,
				"managed_path", asset.ManagedPath,
				"error", verifyErr,
			)
		case AssetStateStaged:
			if err := coordinator.recoverStagedAsset(ctx, asset); err != nil {
				return err
			}
		case AssetStateIntegrityFailed:
			continue
		default:
			return errors.New("recovering assets: unsupported asset state")
		}
	}

	return nil
}

func (coordinator *Coordinator) recoverStagedAsset(
	ctx context.Context,
	asset RecoveryAsset,
) error {
	managedMatches, managedErr := coordinator.store.VerifyManaged(
		ctx, asset.ManagedPath, asset.Digest, asset.Size,
	)
	if managedErr == nil && managedMatches {
		if err := coordinator.repository.MarkAssetReady(
			ctx, asset.ID, asset.ItemID, coordinator.now().UTC(),
		); err != nil {
			return fmt.Errorf("finishing recovered asset: %w", err)
		}
		_ = coordinator.store.RemoveStaged(asset.StagedPath)

		return nil
	}
	stagedMatches, stagedErr := coordinator.store.VerifyStaged(
		ctx, asset.StagedPath, asset.Digest, asset.Size,
	)
	if stagedErr == nil && stagedMatches {
		staged := StagedFile{Path: asset.StagedPath, Digest: asset.Digest, Size: asset.Size}
		promoteErr := coordinator.store.Promote(ctx, staged, asset.ManagedPath)
		if promoteErr == nil {
			if err := coordinator.repository.MarkAssetReady(
				ctx, asset.ID, asset.ItemID, coordinator.now().UTC(),
			); err != nil {
				return fmt.Errorf("finishing promoted recovery asset: %w", err)
			}

			return nil
		}
		if !errors.Is(promoteErr, ErrConflict) {
			return fmt.Errorf("resuming staged asset promotion: %w", promoteErr)
		}
	}
	if managedErr != nil && !errors.Is(managedErr, fs.ErrNotExist) {
		return fmt.Errorf("verifying recovered managed asset: %w", managedErr)
	}
	if stagedErr != nil && !errors.Is(stagedErr, fs.ErrNotExist) {
		return fmt.Errorf("verifying recovered staged asset: %w", stagedErr)
	}
	const message = "neither the staged nor managed asset passed full-digest verification"
	if err := coordinator.repository.FailStagedAsset(
		ctx, asset.ID, asset.ItemID, ErrorCodeIntegrity, message, coordinator.now().UTC(),
	); err != nil {
		return fmt.Errorf("failing unrecoverable staged asset: %w", err)
	}
	coordinator.logger.Error(
		"staged asset could not be recovered",
		"asset_id", asset.ID,
		"managed_error", managedErr,
		"staged_error", stagedErr,
	)

	return nil
}

func (coordinator *Coordinator) resetRecoverableItems(ctx context.Context) error {
	items, err := coordinator.repository.ListRecoverableItems(ctx)
	if err != nil {
		return fmt.Errorf("listing recoverable items: %w", err)
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch item.State {
		case ItemStateAnalyzing:
			const message = "analysis was interrupted by application shutdown"
			if err := coordinator.repository.TransitionImportItem(ctx, ItemTransition{
				ID: item.ID, From: ItemStateAnalyzing, To: ItemStateFailed,
				ErrorCode: ErrorCodeCodexUnavailable, ErrorMessage: message,
				UpdatedAt: coordinator.now().UTC(),
			}); err != nil {
				return fmt.Errorf("failing interrupted analysis: %w", err)
			}
			item.State = ItemStateFailed
			if err := coordinator.store.RemoveAnalysisScratch(item.ID); err != nil {
				return fmt.Errorf("removing interrupted analysis scratch: %w", err)
			}
			fallthrough
		case ItemStateBlocked, ItemStateFailed:
			if item.StagedPath != "" {
				if err := coordinator.store.RemoveStaged(item.StagedPath); err != nil {
					return fmt.Errorf("removing reset staged item: %w", err)
				}
			}
			if err := coordinator.repository.TransitionImportItem(ctx, ItemTransition{
				ID: item.ID, From: item.State, To: ItemStateDiscovered,
				UpdatedAt: coordinator.now().UTC(),
			}); err != nil {
				return fmt.Errorf("resetting recoverable item: %w", err)
			}
		case ItemStateDiscovered, ItemStateStaged, ItemStateCommitting:
			continue
		default:
			return errors.New("recovering items: unsupported item state")
		}
	}

	return nil
}

func (coordinator *Coordinator) recoverSourceDeletions(ctx context.Context) error {
	deletions, err := coordinator.repository.ListPendingDeletions(ctx)
	if err != nil {
		return fmt.Errorf("listing pending source deletions: %w", err)
	}
	for _, deletion := range deletions {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleteErr := coordinator.store.DeleteIncoming(
			ctx, deletion.Path, deletion.Fingerprint, coordinator.config.MaxSourceBytes,
		)
		transition := SourceTransition{
			ID: deletion.ID, From: deletion.SourceState, UpdatedAt: coordinator.now().UTC(),
		}
		switch {
		case deleteErr == nil, errors.Is(deleteErr, fs.ErrNotExist):
			transition.To = SourceStateDeleted
			transition.DeletionState = DeletionStateDeleted
		case errors.Is(deleteErr, ErrSourceChanged):
			transition.To = SourceStateRetained
			transition.DeletionState = DeletionStateNotEligible
			transition.RetainedReason = "source changed before recovered deletion and was retained"
			transition.ErrorCode = ErrorCodeSourceChanged
			transition.ErrorMessage = transition.RetainedReason
		default:
			transition.To = SourceStateRetained
			transition.DeletionState = DeletionStateFailed
			transition.RetainedReason = "source deletion retry failed"
			transition.ErrorCode = ErrorCodeStorage
			transition.ErrorMessage = transition.RetainedReason
		}
		if err := coordinator.repository.TransitionImportSource(ctx, transition); err != nil {
			return fmt.Errorf("recording recovered source deletion: %w", err)
		}
	}

	return nil
}
