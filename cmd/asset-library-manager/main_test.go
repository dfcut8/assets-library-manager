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
