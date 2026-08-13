package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/importer"
)

const (
	promptVersion = "asset-semantic-v1"
	schemaVersion = "asset-semantic-v1"
	providerName  = "openai"
	imageDetail   = "auto"
	maxModelPages = 8
)

// AnalyzerConfig controls the bounded App Server analysis lifecycle.
type AnalyzerConfig struct {
	Command           string
	Model             string
	ReasoningEffort   string
	TurnTimeout       time.Duration
	MaxAttempts       int
	InitialRetryDelay time.Duration
}

// ImageInput identifies one rendition and the durable item it belongs to.
type ImageInput = importer.ImageInput

// AnalysisResult is normalized semantic catalog metadata.
type AnalysisResult = importer.AnalysisResult

// AnalysisProvenance is the accepted immutable AI attempt returned for atomic asset commit.
type AnalysisProvenance = importer.AnalysisProvenance

// AttemptRecorder persists non-accepted attempts as they complete. The accepted attempt is
// returned to the coordinator so it can be committed atomically with the asset.
type AttemptRecorder interface {
	RecordAIRun(context.Context, importer.ID, importer.ID, importer.AIRun) error
}

// Runtime is the lifecycle owned analyzer contract consumed by the application.
type Runtime interface {
	Analyze(context.Context, ImageInput) (AnalysisResult, AnalysisProvenance, error)
	Status() Status
	Close() error
}

// Starter creates one production App Server runtime after the HTTP server is available.
type Starter struct{}

// NewStarter returns the production analyzer starter.
func NewStarter() Starter { return Starter{} }

// Start launches, initializes, and preflights the owned analyzer process.
func (Starter) Start(
	ctx context.Context,
	config AnalyzerConfig,
	recorder AttemptRecorder,
) (Runtime, error) {
	return StartAnalyzer(ctx, config, recorder)
}

// Analyzer owns one reusable Codex App Server transport.
type Analyzer struct {
	config   AnalyzerConfig
	client   *transport
	recorder AttemptRecorder
	status   Status
	now      func() time.Time
	random   func() float64
}

// Status returns the authenticated ChatGPT account state established by preflight.
func (analyzer *Analyzer) Status() Status {
	if analyzer == nil {
		return Status{State: StateUnavailable}
	}

	return analyzer.status
}

// StartAnalyzer starts, initializes, and preflights a reusable analyzer.
func StartAnalyzer(
	ctx context.Context,
	config AnalyzerConfig,
	recorder AttemptRecorder,
) (*Analyzer, error) {
	return startAnalyzer(ctx, config, recorder, startCommand)
}

func startAnalyzer(
	ctx context.Context,
	config AnalyzerConfig,
	recorder AttemptRecorder,
	start startFunc,
) (*Analyzer, error) {
	if err := validateAnalyzerConfig(config); err != nil {
		return nil, err
	}
	if recorder == nil {
		return nil, newAnalysisError(ErrorConfiguration, "AI attempt persistence is unavailable", false, nil)
	}
	client, err := startTransport(ctx, config.Command, start)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	analyzer := &Analyzer{
		config: config, client: client, recorder: recorder,
		now: time.Now, random: rand.Float64,
	}
	if err := analyzer.preflight(ctx); err != nil {
		return nil, errors.Join(err, analyzer.Close())
	}

	return analyzer, nil
}

// Close cooperatively stops and joins the owned App Server process.
func (analyzer *Analyzer) Close() error {
	if analyzer == nil || analyzer.client == nil {
		return nil
	}

	return analyzer.client.Close()
}

// Analyze performs bounded attempts under one overall deadline.
func (analyzer *Analyzer) Analyze(
	ctx context.Context,
	input ImageInput,
) (AnalysisResult, AnalysisProvenance, error) {
	if analyzer == nil || analyzer.client == nil {
		return AnalysisResult{}, AnalysisProvenance{},
			newAnalysisError(ErrorConfiguration, "Codex analyzer is not running", false, nil)
	}
	if err := ctx.Err(); err != nil {
		return AnalysisResult{}, AnalysisProvenance{}, classifyTransportError(err)
	}
	localImagePath, scratchPath, err := validateImageInput(input)
	if err != nil {
		return AnalysisResult{}, AnalysisProvenance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, analyzer.config.TurnTimeout)
	defer cancel()

	for check := 1; check <= analyzer.config.MaxAttempts; check++ {
		err := analyzer.preflightAccountAndLimits(ctx)
		if err == nil {
			break
		}
		classified := asAnalysisError(err)
		if !errors.Is(err, errRateLimited) || check == analyzer.config.MaxAttempts {
			return AnalysisResult{}, AnalysisProvenance{}, classified
		}
		if err := analyzer.waitBeforeRetry(ctx, check, classified.ResetAt); err != nil {
			return AnalysisResult{}, AnalysisProvenance{}, asAnalysisError(err)
		}
	}

	var lastErr error
	for attempt := 1; attempt <= analyzer.config.MaxAttempts; attempt++ {
		startedAt := analyzer.now().UTC()
		runID, idErr := importer.NewID()
		if idErr != nil {
			return AnalysisResult{}, AnalysisProvenance{},
				newAnalysisError(ErrorPermanent, "Could not create analysis provenance", false, idErr)
		}
		run := importer.AIRun{
			ID: runID, Provider: providerName, Model: analyzer.config.Model,
			ReasoningEffort: analyzer.config.ReasoningEffort, ImageDetail: imageDetail,
			PromptVersion: promptVersion, SchemaVersion: schemaVersion,
			AttemptNumber: attempt, StartedAt: startedAt, Outcome: "pending",
		}
		result, normalizedJSON, usageJSON, requestID, attemptErr := analyzer.analyzeOnce(
			ctx, input, localImagePath, scratchPath,
		)
		completedAt := analyzer.now().UTC()
		run.CompletedAt = &completedAt
		run.Latency = completedAt.Sub(startedAt)
		run.UsageJSON = usageJSON
		run.RequestID = requestID
		if attemptErr == nil {
			run.Outcome = "accepted"
			run.NormalizedResultJSON = normalizedJSON

			return result, AnalysisProvenance{Run: run}, nil
		}

		classified := asAnalysisError(attemptErr)
		run.Outcome = outcomeFor(classified, ctx.Err())
		run.ErrorCode = importer.ErrorCodeCodexUnavailable
		run.ErrorMessage = classified.Error()
		recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		recordErr := analyzer.recorder.RecordAIRun(recordCtx, input.ItemID, input.AssetID, run)
		recordCancel()
		if recordErr != nil {
			return AnalysisResult{}, AnalysisProvenance{},
				newAnalysisError(ErrorPermanent, "Could not persist analysis provenance", false, recordErr)
		}
		lastErr = classified
		if ctx.Err() != nil {
			return AnalysisResult{}, AnalysisProvenance{}, classified
		}
		if !classified.Retryable || attempt == analyzer.config.MaxAttempts {
			return AnalysisResult{}, AnalysisProvenance{}, classified
		}
		if err := analyzer.waitBeforeRetry(ctx, attempt, classified.ResetAt); err != nil {
			return AnalysisResult{}, AnalysisProvenance{}, asAnalysisError(err)
		}
	}

	return AnalysisResult{}, AnalysisProvenance{}, lastErr
}

func validateAnalyzerConfig(config AnalyzerConfig) error {
	if strings.TrimSpace(config.Command) == "" || strings.TrimSpace(config.Model) == "" ||
		strings.TrimSpace(config.ReasoningEffort) == "" || config.TurnTimeout <= 0 ||
		config.MaxAttempts < 1 || config.MaxAttempts > 10 || config.InitialRetryDelay <= 0 {
		return newAnalysisError(ErrorConfiguration, "Codex analyzer configuration is invalid", false, nil)
	}

	return nil
}

func validateImageInput(input ImageInput) (string, string, error) {
	if input.ItemID.IsZero() || input.DisplayWidth < 1 || input.DisplayHeight < 1 {
		return "", "", newAnalysisError(ErrorPermanent, "Analysis image metadata is invalid", false, nil)
	}
	if input.Path == "" || input.ScratchDirectory == "" {
		return "", "", newAnalysisError(ErrorPermanent, "Analysis image is unavailable", false, nil)
	}
	scratchInfo, err := os.Lstat(input.ScratchDirectory)
	if err != nil || !scratchInfo.IsDir() || scratchInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", newAnalysisError(ErrorPermanent, "Analysis scratch directory is invalid", false, err)
	}
	imageInfo, err := os.Lstat(input.Path)
	if err != nil || !imageInfo.Mode().IsRegular() || imageInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", newAnalysisError(ErrorPermanent, "Analysis image is invalid", false, err)
	}
	scratchPath, err := filepath.EvalSymlinks(input.ScratchDirectory)
	if err != nil {
		return "", "", newAnalysisError(ErrorPermanent, "Analysis scratch directory is invalid", false, err)
	}
	imagePath, err := filepath.EvalSymlinks(input.Path)
	if err != nil {
		return "", "", newAnalysisError(ErrorPermanent, "Analysis image is invalid", false, err)
	}
	scratchPath, err = filepath.Abs(scratchPath)
	if err != nil {
		return "", "", newAnalysisError(ErrorPermanent, "Analysis scratch directory is invalid", false, err)
	}
	imagePath, err = filepath.Abs(imagePath)
	if err != nil {
		return "", "", newAnalysisError(ErrorPermanent, "Analysis image is invalid", false, err)
	}
	relative, err := filepath.Rel(scratchPath, imagePath)
	if err != nil || relative == "." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." ||
		strings.ContainsRune(relative, filepath.Separator) {
		return "", "", newAnalysisError(
			ErrorPermanent, "Analysis image must be the only item-specific scratch input", false, err,
		)
	}
	entries, err := os.ReadDir(scratchPath)
	if err != nil || len(entries) != 1 || !entries[0].Type().IsRegular() {
		return "", "", newAnalysisError(
			ErrorPermanent, "Analysis scratch directory contains unexpected files", false, err,
		)
	}
	entryInfo, err := entries[0].Info()
	if err != nil || !os.SameFile(imageInfo, entryInfo) {
		return "", "", newAnalysisError(
			ErrorPermanent, "Analysis scratch directory contains an unexpected image", false, err,
		)
	}

	return imagePath, scratchPath, nil
}

func (analyzer *Analyzer) waitBeforeRetry(ctx context.Context, attempt int, resetAt time.Time) error {
	base := analyzer.config.InitialRetryDelay
	for exponent := 1; exponent < attempt && base < time.Hour/2; exponent++ {
		base *= 2
	}
	delay := time.Duration(analyzer.random() * float64(base))
	if untilReset := resetAt.Sub(analyzer.now()); !resetAt.IsZero() && untilReset > delay {
		delay = untilReset
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func asAnalysisError(err error) *AnalysisError {
	var classified *AnalysisError
	if errors.As(err, &classified) {
		return classified
	}

	return classifyTransportError(err)
}

func outcomeFor(err *AnalysisError, contextErr error) string {
	if contextErr != nil {
		return "canceled"
	}
	switch err.Kind {
	case ErrorCanceled, ErrorDeadline:
		return "canceled"
	case ErrorRefused:
		return "refused"
	case ErrorInvalidResponse:
		return "invalid-response"
	case ErrorPermanent, ErrorAuthentication, ErrorConfiguration:
		return "permanent-error"
	default:
		return "retryable-error"
	}
}

func marshalNormalized(value normalizedAnalysis) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding normalized analysis: %w", err)
	}

	return string(data), nil
}
