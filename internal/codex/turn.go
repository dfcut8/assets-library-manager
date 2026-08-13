package codex

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const cleanupTimeout = 3 * time.Second

type threadStartResult struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type turnStartResult struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

type completedItemParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Item     struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Message string `json:"message"`
	} `json:"item"`
}

type completedTurnParams struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID         string          `json:"id"`
		Status     string          `json:"status"`
		TokenUsage json.RawMessage `json:"tokenUsage"`
		Error      *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
}

func (analyzer *Analyzer) analyzeOnce(
	ctx context.Context,
	input ImageInput,
	localImagePath string,
	scratchPath string,
) (
	result AnalysisResult,
	normalizedJSON string,
	usageJSON string,
	requestID string,
	returnErr error,
) {
	var threadResponse threadStartResult
	if err := analyzer.client.request(ctx, "thread/start", map[string]any{
		"model":          analyzer.config.Model,
		"cwd":            scratchPath,
		"approvalPolicy": "never",
		"sandbox":        "readOnly",
		"ephemeral":      true,
		"serviceName":    "asset-library-manager",
	}, &threadResponse); err != nil {
		return AnalysisResult{}, "", "", "", classifyTransportError(err)
	}
	threadID := threadResponse.Thread.ID
	if threadID == "" {
		return AnalysisResult{}, "", "", "", invalidResponse("Codex did not create a classification thread", nil)
	}
	var turnID string
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if ctx.Err() != nil && turnID != "" {
			_ = analyzer.client.request(cleanupCtx, "turn/interrupt", map[string]string{
				"threadId": threadID, "turnId": turnID,
			}, nil)
		}
		if err := analyzer.client.request(
			cleanupCtx, "thread/delete", map[string]string{"threadId": threadID}, nil,
		); err != nil && returnErr == nil {
			returnErr = classifyTransportError(err)
		}
	}()

	collector, stopCollecting := analyzer.client.collect(threadID)
	defer stopCollecting()
	var turnResponse turnStartResult
	if err := analyzer.client.request(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{"type": "text", "text": analysisPrompt},
			{"type": "localImage", "path": localImagePath},
		},
		"model":          analyzer.config.Model,
		"effort":         analyzer.config.ReasoningEffort,
		"approvalPolicy": "never",
		"cwd":            scratchPath,
		"sandboxPolicy": map[string]any{
			"type":          "readOnly",
			"networkAccess": false,
		},
		"outputSchema": outputSchema(),
	}, &turnResponse); err != nil {
		return AnalysisResult{}, "", "", "", classifyTransportError(err)
	}
	turnID = turnResponse.Turn.ID
	if turnID == "" {
		return AnalysisResult{}, "", "", "", invalidResponse("Codex did not create a classification turn", nil)
	}
	requestID = safeRequestIdentifier(threadID, turnID)

	message, usage, err := waitForTurn(ctx, collector, threadID, turnID)
	if err != nil {
		return AnalysisResult{}, "", usage, requestID, err
	}
	result, normalized, err := decodeAndNormalize([]byte(message), input.DisplayWidth, input.DisplayHeight)
	if err != nil {
		return AnalysisResult{}, "", usage, requestID, err
	}
	normalizedJSON, err = marshalNormalized(normalized)
	if err != nil {
		return AnalysisResult{}, "", usage, requestID,
			newAnalysisError(ErrorPermanent, "Could not normalize Codex metadata", false, err)
	}

	return result, normalizedJSON, usage, requestID, nil
}

func safeRequestIdentifier(threadID, turnID string) string {
	if !safeProtocolIdentifier(threadID) || !safeProtocolIdentifier(turnID) {
		return ""
	}
	identifier := threadID + "/" + turnID
	if len(identifier) > 256 {
		return ""
	}

	return identifier
}

func safeProtocolIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == ':' {
			continue
		}

		return false
	}

	return true
}

func waitForTurn(
	ctx context.Context,
	collector *eventCollector,
	threadID string,
	turnID string,
) (string, string, error) {
	var message string
	var usageJSON string
	for received := 0; received < maxCollectedMessages; received++ {
		select {
		case <-ctx.Done():
			return "", usageJSON, ctx.Err()
		case err := <-collector.failed:
			return "", usageJSON, classifyTransportError(err)
		case event := <-collector.messages:
			switch event.Method {
			case "item/completed":
				var params completedItemParams
				if err := json.Unmarshal(event.Params, &params); err != nil {
					return "", usageJSON, invalidResponse("Codex returned a malformed completed item", err)
				}
				if params.ThreadID != threadID || params.TurnID != turnID {
					continue
				}
				switch params.Item.Type {
				case "agentMessage":
					message = params.Item.Text
				case "error":
					return "", usageJSON, classifyTurnFailure("", params.Item.Message)
				}
			case "turn/completed":
				var params completedTurnParams
				if err := json.Unmarshal(event.Params, &params); err != nil {
					return "", usageJSON, invalidResponse("Codex returned a malformed completed turn", err)
				}
				if params.ThreadID != threadID || params.Turn.ID != turnID {
					continue
				}
				usageJSON = safeUsageJSON(params.Turn.TokenUsage)
				if params.Turn.Status != "completed" {
					if params.Turn.Error == nil {
						return "", usageJSON, classifyTurnFailure(params.Turn.Status, "")
					}
					return "", usageJSON,
						classifyTurnFailure(params.Turn.Error.Code, params.Turn.Error.Message)
				}
				if strings.TrimSpace(message) == "" {
					return "", usageJSON, invalidResponse("Codex completed without semantic metadata", nil)
				}

				return message, usageJSON, nil
			}
		}
	}

	return "", usageJSON, invalidResponse("Codex emitted too many events", nil)
}

func classifyTurnFailure(code string, message string) *AnalysisError {
	combined := strings.ToLower(code + " " + message)
	if strings.Contains(combined, "refus") || strings.Contains(combined, "safety") {
		return newAnalysisError(ErrorRefused, "Codex declined to classify this image", false, nil)
	}
	if strings.Contains(combined, "invalid") || strings.Contains(combined, "schema") {
		return invalidResponse("Codex returned invalid structured metadata", nil)
	}
	if strings.Contains(combined, "auth") || strings.Contains(combined, "unauthorized") {
		return newAnalysisError(ErrorAuthentication, "Codex authentication is unavailable", false, nil)
	}
	if strings.Contains(combined, "rate") || strings.Contains(combined, "limit") {
		return newAnalysisError(ErrorRetryable, "Codex rate limit reached", true, nil)
	}
	if code == "interrupted" {
		return newAnalysisError(ErrorRetryable, "Codex turn was interrupted", true, nil)
	}

	if safeCode := safeTurnFailureCode(code); safeCode != "" {
		return newAnalysisError(ErrorRetryable,
			"Codex could not complete classification (code "+safeCode+")", true, nil)
	}

	return newAnalysisError(ErrorRetryable, "Codex could not complete classification", true, nil)
}

func safeTurnFailureCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 128 {
		return ""
	}
	for _, char := range code {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return ""
	}

	return code
}
