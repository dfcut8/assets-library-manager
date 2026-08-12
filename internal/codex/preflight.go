package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type supportedEffort struct {
	Name string
}

func (effort *supportedEffort) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		effort.Name = name
		return nil
	}
	var object struct {
		ReasoningEffort string `json:"reasoningEffort"`
		Effort          string `json:"effort"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	effort.Name = object.ReasoningEffort
	if effort.Name == "" {
		effort.Name = object.Effort
	}

	return nil
}

type modelInfo struct {
	ID                        string            `json:"id"`
	Model                     string            `json:"model"`
	SupportedReasoningEfforts []supportedEffort `json:"supportedReasoningEfforts"`
	InputModalities           []string          `json:"inputModalities"`
}

type modelListResult struct {
	Data       []modelInfo `json:"data"`
	Models     []modelInfo `json:"models"`
	NextCursor string      `json:"nextCursor"`
}

type rateLimitWindow struct {
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    int64   `json:"resetsAt"`
	ResetAt     int64   `json:"resetAt"`
}

type rateLimits struct {
	RateLimitReachedType string           `json:"rateLimitReachedType"`
	Primary              *rateLimitWindow `json:"primary"`
	Secondary            *rateLimitWindow `json:"secondary"`
}

type rateLimitsResult struct {
	RateLimits *rateLimits `json:"rateLimits"`
}

func (analyzer *Analyzer) preflight(ctx context.Context) error {
	if err := analyzer.preflightAccountAndLimits(ctx); err != nil && !errors.Is(err, errRateLimited) {
		return err
	}
	model, err := analyzer.findModel(ctx)
	if err != nil {
		return err
	}
	if len(model.InputModalities) > 0 && !containsFold(model.InputModalities, "image") {
		return newAnalysisError(
			ErrorConfiguration, "Configured Codex model does not accept images", false, nil,
		)
	}
	for _, effort := range model.SupportedReasoningEfforts {
		if effort.Name == analyzer.config.ReasoningEffort {
			return nil
		}
	}

	return newAnalysisError(
		ErrorConfiguration, "Configured Codex reasoning effort is unsupported", false, nil,
	)
}

func (analyzer *Analyzer) preflightAccountAndLimits(ctx context.Context) error {
	var accountResponse accountResult
	if err := analyzer.client.request(
		ctx, "account/read", map[string]bool{"refreshToken": false}, &accountResponse,
	); err != nil {
		return classifyTransportError(err)
	}
	if accountResponse.Account == nil {
		return newAnalysisError(ErrorAuthentication, "Sign in to ChatGPT in Codex", false, nil)
	}
	if accountResponse.Account.Type != "chatgpt" {
		return newAnalysisError(
			ErrorAuthentication, "A ChatGPT-backed Codex account is required", false, nil,
		)
	}

	var limits rateLimitsResult
	if err := analyzer.client.request(ctx, "account/rateLimits/read", map[string]any{}, &limits); err != nil {
		return classifyTransportError(err)
	}
	if limits.RateLimits == nil {
		return nil
	}
	limited := limits.RateLimits.RateLimitReachedType != ""
	var resetAt time.Time
	for _, window := range []*rateLimitWindow{limits.RateLimits.Primary, limits.RateLimits.Secondary} {
		if window == nil {
			continue
		}
		limited = limited || window.UsedPercent >= 100
		seconds := window.ResetsAt
		if seconds == 0 {
			seconds = window.ResetAt
		}
		candidate := time.Unix(seconds, 0).UTC()
		if seconds > 0 && candidate.After(resetAt) {
			resetAt = candidate
		}
	}
	if !limited {
		return nil
	}
	err := newAnalysisError(ErrorRetryable, "Codex rate limit reached", true, errRateLimited)
	err.ResetAt = resetAt

	return err
}

func (analyzer *Analyzer) findModel(ctx context.Context) (modelInfo, error) {
	cursor := ""
	for page := 0; page < maxModelPages; page++ {
		params := map[string]any{"limit": 100, "includeHidden": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var response modelListResult
		if err := analyzer.client.request(ctx, "model/list", params, &response); err != nil {
			return modelInfo{}, classifyTransportError(err)
		}
		models := append(response.Data, response.Models...)
		for _, model := range models {
			identifier := model.ID
			if identifier == "" {
				identifier = model.Model
			}
			if identifier == analyzer.config.Model {
				return model, nil
			}
		}
		if response.NextCursor == "" {
			break
		}
		if response.NextCursor == cursor {
			return modelInfo{}, newAnalysisError(
				ErrorConfiguration, "Codex model pagination did not advance", false, nil,
			)
		}
		cursor = response.NextCursor
	}

	return modelInfo{}, newAnalysisError(
		ErrorConfiguration, fmt.Sprintf("Configured Codex model %q is unavailable", analyzer.config.Model), false, nil,
	)
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}

	return false
}
