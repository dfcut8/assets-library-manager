package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionIsSideEffectFree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version) = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "asset-library-manager") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestUnexpectedArgumentReturnsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"unexpected"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(unexpected) = %d, want 2", code)
	}
}

func TestNewLoggerUsesHumanReadableText(t *testing.T) {
	var output bytes.Buffer
	newLogger(&output).Info("local server ready", "url", "http://127.0.0.1:7342/")

	got := output.String()
	if !strings.Contains(got, "level=INFO") || !strings.Contains(got, "msg=\"local server ready\"") ||
		!strings.Contains(got, "url=http://127.0.0.1:7342/") || strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Fatalf("log output = %q, want human-readable slog text", got)
	}
}

func TestNewLoggerEnablesProtocolDiagnosticsByDefault(t *testing.T) {
	t.Setenv("ASSET_LIBRARY_MANAGER_LOG_LEVEL", "")
	var output bytes.Buffer
	newLogger(&output).Debug("codex app server request started", "rpc_method", "turn/start")

	got := output.String()
	if !strings.Contains(got, "level=DEBUG") || !strings.Contains(got, "rpc_method=turn/start") {
		t.Fatalf("debug log output = %q", got)
	}
}

func TestNewLoggerCanDisableProtocolDiagnostics(t *testing.T) {
	t.Setenv("ASSET_LIBRARY_MANAGER_LOG_LEVEL", "info")
	var output bytes.Buffer
	logger := newLogger(&output)
	logger.Debug("codex app server request started", "rpc_method", "turn/start")
	logger.Info("local server ready")

	got := output.String()
	if strings.Contains(got, "level=DEBUG") || !strings.Contains(got, "level=INFO") {
		t.Fatalf("log output = %q, want info without debug", got)
	}
}
