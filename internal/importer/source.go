package importer

import (
	"context"
	"errors"
)

func (coordinator *Coordinator) finalizeSource(ctx context.Context, source *sourceRun) {
	if source.finalized || ctx.Err() != nil {
		return
	}
	source.finalized = true
	summary, err := coordinator.repository.SummarizeSource(ctx, source.record.ID)
	if err != nil {
		coordinator.addFailure(ProgressFailure{
			Source: source.record.Path.String(), Code: ErrorCodeInternal,
			Message: "source outcome could not be summarized",
		})
		coordinator.completeSource(false, true)

		return
	}
	eligible := coordinator.sourceEligible(source, summary)
	state, reason, code := aggregateSourceState(source, summary, eligible)
	deletionState := DeletionStateNotEligible
	if eligible {
		deletionState = DeletionStateEligible
	}
	if err := coordinator.transitionSource(
		ctx, &source.record, state, deletionState, reason, code, reason,
	); err != nil {
		coordinator.addFailure(ProgressFailure{
			Source: source.record.Path.String(), Code: ErrorCodeInternal,
			Message: "source outcome could not be persisted",
		})
		coordinator.completeSource(false, true)

		return
	}
	if !eligible {
		coordinator.completeSource(false, true)
		return
	}
	coordinator.deleteEligibleSource(ctx, source)
}

func (coordinator *Coordinator) sourceEligible(source *sourceRun, summary SourceSummary) bool {
	allSuccessful := summary.Total > 0 && summary.Ready+summary.Duplicate == summary.Total
	if !allSuccessful || source.scanFailure != "" {
		return false
	}
	switch source.kind {
	case CandidateKindLoose:
		return summary.Total == 1
	case CandidateKindZIP:
		return source.supported > 0 && source.unhandled == 0 && summary.Total == source.supported
	default:
		return false
	}
}

func aggregateSourceState(
	source *sourceRun,
	summary SourceSummary,
	eligible bool,
) (SourceState, string, ErrorCode) {
	if eligible {
		if summary.Ready == 0 {
			return SourceStateDuplicate, "", ""
		}

		return SourceStateReady, "", ""
	}
	if source.kind == CandidateKindUnsupported {
		return SourceStateFailed, "source extension is unsupported", ErrorCodeInvalidInput
	}
	if source.scanFailure != "" {
		return SourceStateRetained, source.scanFailure, ErrorCodeInvalidInput
	}
	if source.kind == CandidateKindZIP && source.supported == 0 {
		return SourceStateRetained, "archive contains no supported images", ErrorCodeInvalidInput
	}
	if source.unhandled > 0 {
		return SourceStateRetained, "archive contains unhandled regular files", ErrorCodeInvalidInput
	}
	if summary.Blocked > 0 {
		return SourceStateBlocked, "one or more images are blocked", ErrorCodeCodexUnavailable
	}
	if summary.Failed > 0 {
		return SourceStateFailed, "one or more images failed", ErrorCodeInvalidInput
	}

	return SourceStateRetained, "source is not eligible for deletion", ErrorCodeInvalidInput
}

func (coordinator *Coordinator) deleteEligibleSource(ctx context.Context, source *sourceRun) {
	if err := coordinator.transitionSource(
		ctx, &source.record, source.record.State, DeletionStatePending, "", "", "",
	); err != nil {
		coordinator.completeSource(false, true)
		return
	}
	err := coordinator.store.DeleteIncoming(
		ctx, source.record.Path, source.record.DiscoveryFingerprint, coordinator.config.MaxSourceBytes,
	)
	if err == nil {
		if transitionErr := coordinator.transitionSource(
			ctx, &source.record, SourceStateDeleted, DeletionStateDeleted, "", "", "",
		); transitionErr != nil {
			coordinator.addFailure(ProgressFailure{
				Source: source.record.Path.String(), Code: ErrorCodeInternal,
				Message: "source deletion could not be recorded",
			})
			coordinator.completeSource(false, true)

			return
		}
		coordinator.completeSource(true, false)

		return
	}
	if errors.Is(err, ErrSourceChanged) {
		const message = "source changed before deletion and was retained"
		_ = coordinator.transitionSource(
			ctx, &source.record, SourceStateRetained, DeletionStateNotEligible,
			message, ErrorCodeSourceChanged, message,
		)
		coordinator.addFailure(ProgressFailure{
			Source: source.record.Path.String(), Code: ErrorCodeSourceChanged, Message: message,
		})
		coordinator.completeSource(false, true)

		return
	}
	const message = "source deletion failed and will be retried at next startup"
	_ = coordinator.transitionSource(
		ctx, &source.record, SourceStateRetained, DeletionStateFailed,
		message, ErrorCodeStorage, message,
	)
	coordinator.addFailure(ProgressFailure{
		Source: source.record.Path.String(), Code: ErrorCodeStorage, Message: message,
	})
	coordinator.completeSource(false, true)
}

func (coordinator *Coordinator) transitionSource(
	ctx context.Context,
	record *SourceRecord,
	to SourceState,
	deletion DeletionState,
	reason string,
	code ErrorCode,
	message string,
) error {
	from := record.State
	if err := coordinator.repository.TransitionImportSource(ctx, SourceTransition{
		ID: record.ID, From: from, To: to, DeletionState: deletion,
		RetainedReason: reason, ErrorCode: code, ErrorMessage: message,
		UpdatedAt: coordinator.now().UTC(),
	}); err != nil {
		return err
	}
	record.State = to
	record.DeletionState = deletion
	record.RetainedReason = reason
	record.ErrorCode = code
	record.ErrorMessage = message
	record.UpdatedAt = coordinator.now().UTC()

	return nil
}

func (coordinator *Coordinator) completeSource(deleted, retained bool) {
	coordinator.updateProgress(func(progress *Progress) {
		progress.SourcesCompleted++
		if deleted {
			progress.SourcesDeleted++
		}
		if retained {
			progress.SourcesRetained++
		}
	})
}
