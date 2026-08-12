//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/codex"
	"github.com/dfcut8/assets-library-manager/internal/config"
	"github.com/dfcut8/assets-library-manager/internal/storage"
)

func TestRunBootstrapsServesLaunchesAndShutsDown(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Server.Port = availablePort(t)
	writeApplicationConfig(t, root, cfg)
	launcher := &recordingLauncher{called: make(chan string, 1)}
	application := New(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		launcher,
		staticCodexChecker{status: codex.Status{State: codex.StateReady, PlanType: "plus"}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- application.Run(ctx, root)
	}()

	select {
	case targetURL := <-launcher.called:
		if !strings.Contains(targetURL, "127.0.0.1") {
			t.Fatalf("browser URL = %q", targetURL)
		}
		cancel()
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("application did not reach browser launch")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("application did not shut down")
	}
	if _, err := os.Stat(filepath.Join(root, "assets.db")); err != nil {
		t.Fatalf("database not created: %v", err)
	}
}

func TestRunRefusesDatabaseRemovalWithRetainedProcessedData(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Server.Port = availablePort(t)
	writeApplicationConfig(t, root, cfg)
	launcher := &recordingLauncher{called: make(chan string, 1)}
	application := New(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		launcher,
		staticCodexChecker{status: codex.Status{State: codex.StateReady, PlanType: "plus"}},
	)

	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- application.Run(firstCtx, root)
	}()
	select {
	case <-launcher.called:
		firstCancel()
	case <-time.After(10 * time.Second):
		firstCancel()
		t.Fatal("initial application run did not start")
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("initial Run() error = %v", err)
	}
	managedPath := filepath.Join(root, "processed", "vehicle", "asset.png")
	if err := os.MkdirAll(filepath.Dir(managedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("retained managed bytes")
	if err := os.WriteFile(managedPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "assets.db")); err != nil {
		t.Fatal(err)
	}

	err := application.Run(context.Background(), root)
	var recoveryErr *storage.DatabaseRecoveryError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("Run() error = %v, want DatabaseRecoveryError", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "assets.db")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database recreated after refusal: %v", statErr)
	}
	got, readErr := os.ReadFile(managedPath)
	if readErr != nil || string(got) != string(want) {
		t.Fatalf("managed data changed: got=%q error=%v", got, readErr)
	}
}

type recordingLauncher struct {
	mu     sync.Mutex
	called chan string
}

type staticCodexChecker struct {
	status codex.Status
	err    error
}

func (c staticCodexChecker) Check(context.Context, string) (codex.Status, error) {
	return c.status, c.err
}

func (l *recordingLauncher) Open(_ context.Context, targetURL string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.called != nil {
		l.called <- targetURL
	}

	return errors.New("test browser unavailable")
}

func availablePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	return port
}

func writeApplicationConfig(t *testing.T, root string, cfg config.Config) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, config.FileName), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
