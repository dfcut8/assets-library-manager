//go:build integration

package codex

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestChecker_CheckInstalledCodex(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex command is not installed on PATH")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	status, err := New().Check(ctx, "codex")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if status.State == "" || status.State == StateUnavailable {
		t.Fatalf("Check() state = %q, want a resolved account state", status.State)
	}
}
