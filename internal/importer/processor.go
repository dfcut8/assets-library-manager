package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/dfcut8/assets-library-manager/internal/imageinspect"
)

type itemProcessingError struct {
	code    ErrorCode
	message string
	cause   error
}

func (err *itemProcessingError) Error() string { return err.message }
func (err *itemProcessingError) Unwrap() error { return err.cause }

func itemError(code ErrorCode, message string, cause error) error {
	return &itemProcessingError{code: code, message: message, cause: cause}
}

func (coordinator *Coordinator) worker(
	ctx context.Context,
	queue <-chan workItem,
	results chan<- itemResult,
) {
	for work := range queue {
		result := coordinator.processItem(ctx, work)
		select {
		case results <- result:
		case <-ctx.Done():
			return
		}
	}
}

func (coordinator *Coordinator) processItem(ctx context.Context, work workItem) itemResult {
	result := itemResult{
		sourceID: work.source.ID,
		itemID:   work.item.ID,
		path:     work.sourcePath,
		itemName: work.item.ZIPEntryName,
	}
	if err := ctx.Err(); err != nil {
		return result
	}
	item := work.item
	staged := work.staged
	if item.State == ItemStateDiscovered {
		var err error
		staged, err = coordinator.stageLoose(ctx, work)
		if err != nil {
			code, message := classifyProcessingFailure(err)
			if code == ErrorCodeCodexUnavailable {
				code, message = ErrorCodeStorage, "image could not be staged"
			}
			return coordinator.failedResult(ctx, work, item, code, message, err)
		}
		item.State = ItemStateStaged
		item.StagedPath = staged.Path
		item.Digest = staged.Digest
	}

	if existing, err := coordinator.repository.FindReadyByDigest(ctx, staged.Digest); err == nil {
		return coordinator.duplicateResult(ctx, work, item, existing)
	} else if !errors.Is(err, ErrNotFound) {
		return coordinator.failedResult(ctx, work, item, ErrorCodeStorage,
			"catalog deduplication could not be checked", err)
	}

	leader, reservation, err := coordinator.reserve(ctx, staged.Digest)
	if err != nil {
		if reservation != nil && !reservation.asset.ID.IsZero() {
			return coordinator.duplicateResult(ctx, work, item, reservation.asset)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result
		}

		code, message := classifyProcessingFailure(err)
		return coordinator.failedResult(ctx, work, item, code, message, err)
	}
	if !leader {
		return coordinator.duplicateResult(ctx, work, item, reservation.asset)
	}

	asset, tokenUsage, processErr := coordinator.processReservedItem(ctx, work, &item, staged)
	result.tokenUsage = tokenUsage
	coordinator.finishReservation(staged.Digest, reservation, asset, processErr)
	if processErr != nil {
		if errors.Is(processErr, context.Canceled) || errors.Is(processErr, context.DeadlineExceeded) {
			return result
		}
		if item.State == ItemStateCommitting {
			result.code = ErrorCodeStorage
			result.message = "asset commit will be recovered at next startup"

			return result
		}
		code, message := classifyProcessingFailure(processErr)

		return coordinator.failedResult(ctx, work, item, code, message, processErr)
	}
	result.state = ItemStateReady

	return result
}

func (coordinator *Coordinator) stageLoose(ctx context.Context, work workItem) (StagedFile, error) {
	source, err := coordinator.store.OpenIncoming(work.sourcePath)
	if err != nil {
		return StagedFile{}, fmt.Errorf("opening loose image: %w", err)
	}
	staged, stageErr := coordinator.store.Stage(ctx, work.item.ID, source, coordinator.config.MaxSourceBytes)
	closeErr := source.Close()
	if stageErr != nil || closeErr != nil {
		return StagedFile{}, errors.Join(stageErr, closeErr)
	}
	if staged.Digest != work.source.DiscoveryFingerprint {
		_ = coordinator.store.RemoveStaged(staged.Path)

		return StagedFile{}, ErrSourceChanged
	}
	if err := coordinator.repository.TransitionImportItem(ctx, ItemTransition{
		ID: work.item.ID, From: ItemStateDiscovered, To: ItemStateStaged,
		StagedPath: staged.Path, Digest: staged.Digest, UpdatedAt: coordinator.now().UTC(),
	}); err != nil {
		_ = coordinator.store.RemoveStaged(staged.Path)

		return StagedFile{}, err
	}

	return staged, nil
}

func (coordinator *Coordinator) processReservedItem(
	ctx context.Context,
	work workItem,
	item *ItemRecord,
	staged StagedFile,
) (AssetRef, TokenUsage, error) {
	file, err := coordinator.store.OpenStaged(staged.Path)
	if err != nil {
		return AssetRef{}, TokenUsage{}, fmt.Errorf("opening staged image: %w", err)
	}
	limits := coordinator.config.InspectionLimits
	limits.ExpectedExtension = work.extension
	inspection, inspectErr := coordinator.inspector.Inspect(ctx, file, limits)
	closeErr := file.Close()
	if inspectErr != nil || closeErr != nil {
		joined := errors.Join(inspectErr, closeErr)
		if inspectErr != nil {
			return AssetRef{}, TokenUsage{}, itemError(
				ErrorCodeInvalidInput,
				"image is invalid or exceeds configured safety limits",
				joined,
			)
		}
		return AssetRef{}, TokenUsage{}, itemError(
			ErrorCodeStorage, "staged image could not be closed", joined,
		)
	}
	if err := coordinator.repository.TransitionImportItem(ctx, ItemTransition{
		ID: item.ID, From: ItemStateStaged, To: ItemStateAnalyzing,
		UpdatedAt: coordinator.now().UTC(),
	}); err != nil {
		return AssetRef{}, TokenUsage{}, itemError(
			ErrorCodeStorage, "analysis state could not be recorded", err,
		)
	}
	item.State = ItemStateAnalyzing

	scratch, err := coordinator.store.CreateAnalysisScratch(
		item.ID, inspection.Analysis.Extension, inspection.Analysis.Data,
	)
	if err != nil {
		return AssetRef{}, TokenUsage{}, itemError(
			ErrorCodeStorage, "analysis rendition could not be staged", err,
		)
	}
	defer func() {
		if removeErr := coordinator.store.RemoveAnalysisScratch(item.ID); removeErr != nil {
			coordinator.logger.Warn("removing analysis scratch", "item_id", item.ID, "error", removeErr)
		}
	}()
	analysis, provenance, err := coordinator.analyzer.Analyze(ctx, ImageInput{
		ItemID: item.ID, Path: scratch.Path, ScratchDirectory: scratch.Directory,
		DisplayWidth: inspection.DisplayWidth, DisplayHeight: inspection.DisplayHeight,
	})
	if err != nil {
		return AssetRef{}, provenance.TokenUsage, itemError(
			ErrorCodeCodexUnavailable,
			"semantic analysis failed; check Codex configuration and restart",
			err,
		)
	}
	assetID, err := newWorkflowID()
	if err != nil {
		return AssetRef{}, provenance.TokenUsage, itemError(
			ErrorCodeInternal, "asset identifier could not be generated", err,
		)
	}
	managedPath, err := managedAssetPath(analysis, inspection.Format, staged.Digest)
	if err != nil {
		return AssetRef{}, provenance.TokenUsage, itemError(
			ErrorCodeInternal, "managed asset path could not be created", err,
		)
	}
	colors, err := json.Marshal(inspection.DominantColors)
	if err != nil {
		return AssetRef{}, provenance.TokenUsage, itemError(
			ErrorCodeInternal, "dominant colors could not be encoded", err,
		)
	}
	now := coordinator.now().UTC()
	asset := StagedAsset{
		ID: assetID, ItemID: item.ID, Digest: staged.Digest,
		OriginalFilename: work.originalName, ManagedPath: managedPath,
		Format: inspection.Format, MIMEType: inspection.MIMEType,
		FileSizeBytes: inspection.FileSizeBytes,
		DisplayWidth:  inspection.DisplayWidth, DisplayHeight: inspection.DisplayHeight,
		OrientationClass: inspection.OrientationClass,
		HasAlpha:         inspection.HasAlpha, HasTransparency: inspection.HasTransparency,
		EncodedAnimated:    inspection.EncodedAnimated,
		EncodedFrameCount:  inspection.EncodedFrameCount,
		DominantColorsJSON: string(colors),
		Title:              analysis.Title, Description: analysis.Description,
		PrimaryType: analysis.PrimaryType, Style: analysis.Style,
		PixelArt: analysis.PixelArt, AIConfidence: analysis.Confidence,
		Layout: analysis.Layout, SearchTags: flattenedTags(analysis.Tags),
		Thumbnail: Thumbnail{
			Width: inspection.Thumbnail.Width, Height: inspection.Thumbnail.Height,
			Data: append([]byte(nil), inspection.Thumbnail.Data...),
		},
		Tags: append([]Tag(nil), analysis.Tags...), AIRun: provenance.Run, CreatedAt: now,
	}
	if err := coordinator.repository.CommitStagedAsset(ctx, asset); err != nil {
		return AssetRef{}, provenance.TokenUsage, itemError(
			ErrorCodeStorage, "asset metadata could not be committed", err,
		)
	}
	item.State = ItemStateCommitting
	item.AssetID = assetID
	if err := coordinator.store.Promote(ctx, staged, managedPath); err != nil {
		return AssetRef{}, provenance.TokenUsage, itemError(
			ErrorCodeStorage, "managed original could not be promoted", err,
		)
	}
	if err := coordinator.repository.MarkAssetReady(ctx, assetID, item.ID, coordinator.now().UTC()); err != nil {
		return AssetRef{}, provenance.TokenUsage, itemError(
			ErrorCodeStorage, "asset readiness could not be committed", err,
		)
	}
	item.State = ItemStateReady

	return AssetRef{
		ID: assetID, Digest: staged.Digest, ManagedPath: managedPath,
	}, provenance.TokenUsage, nil
}

func (coordinator *Coordinator) duplicateResult(
	ctx context.Context,
	work workItem,
	item ItemRecord,
	asset AssetRef,
) itemResult {
	result := itemResult{
		sourceID: work.source.ID, itemID: work.item.ID, path: work.sourcePath,
		itemName: work.item.ZIPEntryName, state: ItemStateDuplicate,
	}
	if asset.ID.IsZero() {
		result.state = ItemStateFailed
		result.code = ErrorCodeInternal
		result.message = "matching image result is unavailable"

		return result
	}
	if err := coordinator.repository.TransitionImportItem(ctx, ItemTransition{
		ID: item.ID, From: item.State, To: ItemStateDuplicate,
		AssetID: asset.ID, UpdatedAt: coordinator.now().UTC(),
	}); err != nil {
		return coordinator.failedResult(ctx, work, item, ErrorCodeStorage,
			"duplicate image could not be recorded", err)
	}
	if item.StagedPath != "" {
		if err := coordinator.store.RemoveStaged(item.StagedPath); err != nil {
			coordinator.logger.Warn("removing duplicate staged image", "item_id", item.ID, "error", err)
		}
	}

	return result
}

func (coordinator *Coordinator) failItem(
	ctx context.Context,
	item ItemRecord,
	code ErrorCode,
	message string,
) error {
	if err := coordinator.repository.TransitionImportItem(ctx, ItemTransition{
		ID: item.ID, From: item.State, To: ItemStateFailed,
		ErrorCode: code, ErrorMessage: message, UpdatedAt: coordinator.now().UTC(),
	}); err != nil {
		return err
	}

	return nil
}

func (coordinator *Coordinator) failedResult(
	ctx context.Context,
	work workItem,
	item ItemRecord,
	code ErrorCode,
	message string,
	cause error,
) itemResult {
	coordinator.logger.Debug(
		"import item processing failure details",
		"source", work.sourcePath,
		"item", work.item.ZIPEntryName,
		"item_id", item.ID,
		"code", code,
		"message", message,
		"error", processingFailureDiagnostic(cause),
	)
	result := itemResult{
		sourceID: work.source.ID, path: work.sourcePath, itemName: work.item.ZIPEntryName,
		state: ItemStateFailed, code: code, message: message,
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		result.state = ItemStateUnknown
		result.code = ""
		result.message = ""

		return result
	}
	if err := coordinator.failItem(ctx, item, code, message); err != nil {
		coordinator.logger.Error("persisting item failure", "item_id", item.ID, "error", err)
		result.code = ErrorCodeInternal
		result.message = "item failure could not be persisted"
	}

	return result
}

func processingFailureDiagnostic(err error) error {
	var processingErr *itemProcessingError
	if errors.As(err, &processingErr) && processingErr.cause != nil {
		return processingErr.cause
	}

	return err
}

func classifyProcessingFailure(err error) (ErrorCode, string) {
	var processingErr *itemProcessingError
	if errors.As(err, &processingErr) {
		return processingErr.code, processingErr.message
	}
	switch {
	case errors.Is(err, ErrSourceChanged):
		return ErrorCodeSourceChanged,
			"source bytes changed after discovery; restart to reconsider the current file"
	case errors.Is(err, imageinspect.ErrUnsupportedFormat),
		errors.Is(err, imageinspect.ErrFormatMismatch),
		errors.Is(err, imageinspect.ErrSourceLimit),
		errors.Is(err, imageinspect.ErrPixelLimit),
		errors.Is(err, imageinspect.ErrInvalidMetadata),
		errors.Is(err, imageinspect.ErrDecoderPanic),
		errors.Is(err, imageinspect.ErrRenditionLimit):
		return ErrorCodeInvalidInput, "image is invalid or exceeds configured safety limits"
	default:
		return ErrorCodeCodexUnavailable,
			"semantic analysis failed; check Codex configuration and restart"
	}
}

func managedAssetPath(
	analysis AnalysisResult,
	format string,
	digest Digest,
) (ManagedPath, error) {
	extension := format
	if extension == "jpeg" {
		extension = "jpg"
	}
	name := slugFilename(analysis.Title)
	if name == "" {
		name = "asset"
	}
	value := path.Join(string(analysis.PrimaryType), name+"--"+digest.String()[:12]+"."+extension)

	return NewManagedPath(value)
}

func slugFilename(value string) string {
	var builder strings.Builder
	separator := false
	for _, character := range strings.ToLower(value) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			if separator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			builder.WriteRune(character)
			separator = false
		} else {
			separator = true
		}
		if builder.Len() >= 80 {
			break
		}
	}

	return strings.Trim(builder.String(), "-.")
}

func flattenedTags(tags []Tag) string {
	values := make([]string, 0, len(tags))
	for _, tag := range tags {
		values = append(values, tag.Facet+":"+tag.Slug)
	}
	sort.Strings(values)

	return strings.Join(values, " ")
}
