package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxDiagnosticBytes = 2048

var errRateLimited = errors.New("codex rate limited")

// ErrorKind is the stable category exposed to the import coordinator.
type ErrorKind string

// Stable analysis error categories let the coordinator distinguish retry policy and user guidance.
const (
	ErrorRetryable       ErrorKind = "retryable"
	ErrorPermanent       ErrorKind = "permanent"
	ErrorRefused         ErrorKind = "refused"
	ErrorInvalidResponse ErrorKind = "invalid-response"
	ErrorAuthentication  ErrorKind = "authentication"
	ErrorConfiguration   ErrorKind = "configuration"
	ErrorCanceled        ErrorKind = "canceled"
	ErrorDeadline        ErrorKind = "deadline"
)

// AnalysisError classifies a safe external-service failure.
type AnalysisError struct {
	Kind      ErrorKind
	ResetAt   time.Time
	Message   string
	Retryable bool
	cause     error
}

func (err *AnalysisError) Error() string {
	if err == nil {
		return ""
	}
	message := sanitizeDiagnostic(err.Message)
	if message == "" {
		message = "semantic analysis failed"
	}

	return fmt.Sprintf("codex: %s: %s", err.Kind, message)
}

// Unwrap returns the internal cause without exposing it in Error's user-safe text.
func (err *AnalysisError) Unwrap() error { return err.cause }

func newAnalysisError(kind ErrorKind, message string, retryable bool, cause error) *AnalysisError {
	return &AnalysisError{Kind: kind, Message: message, Retryable: retryable, cause: cause}
}

func sanitizeDiagnostic(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	builder.Grow(min(len(value), maxDiagnosticBytes))
	previousSpace := false
	for _, char := range value {
		if unicode.IsControl(char) {
			char = ' '
		}
		isSpace := unicode.IsSpace(char)
		if isSpace {
			if previousSpace {
				continue
			}
			char = ' '
		}
		if builder.Len()+utf8.RuneLen(char) > maxDiagnosticBytes {
			break
		}
		builder.WriteRune(char)
		previousSpace = isSpace
	}

	return strings.TrimSpace(builder.String())
}

func classifyTransportError(err error) *AnalysisError {
	if errors.Is(err, context.DeadlineExceeded) {
		return newAnalysisError(ErrorDeadline, "Codex timed out", false, err)
	}
	if errors.Is(err, context.Canceled) {
		return newAnalysisError(ErrorCanceled, "Codex analysis was canceled", false, err)
	}
	var remote *rpcError
	if errors.As(err, &remote) {
		method := ""
		var request *rpcRequestError
		if errors.As(err, &request) {
			method = request.Method
		}
		detail := rpcDiagnostic(method, remote)
		switch remote.Code {
		case -32001, -32002, 401, 403:
			return newAnalysisError(ErrorAuthentication, "Codex authentication is unavailable"+detail, false, err)
		case -32601, -32602:
			return newAnalysisError(ErrorConfiguration, "Codex App Server is incompatible"+detail, false, err)
		case 429:
			return newAnalysisError(ErrorRetryable, "Codex rate limit reached"+detail, true, err)
		default:
			return newAnalysisError(ErrorRetryable,
				fmt.Sprintf("Codex App Server request failed (RPC code %d)%s", remote.Code, detail), true, err)
		}
	}

	return newAnalysisError(ErrorRetryable, "Codex became unavailable", true, err)
}

func rpcDiagnostic(method string, remote *rpcError) string {
	parts := make([]string, 0, 2)
	if safeRPCMethod(method) {
		parts = append(parts, "method "+method)
	}
	if message := sanitizeDiagnostic(remote.Message); message != "" {
		parts = append(parts, "message "+message)
	}
	if len(parts) == 0 {
		return ""
	}

	return "; " + strings.Join(parts, "; ")
}

func safeRPCMethod(method string) bool {
	if method == "" || len(method) > 128 {
		return false
	}
	for _, char := range method {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '/' || char == '-' || char == '_' || char == '.' {
			continue
		}

		return false
	}

	return true
}
