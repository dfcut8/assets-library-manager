//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/codex"
	"github.com/dfcut8/assets-library-manager/internal/config"
	"github.com/dfcut8/assets-library-manager/internal/platform"
	"github.com/dfcut8/assets-library-manager/internal/sqlite"
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
		platform.FileLauncher{},
		staticCodexStarter{},
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

func TestRunPreservesRequestProtectionAcrossHandlerUpdates(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Server.Port = availablePort(t)
	writeApplicationConfig(t, root, cfg)
	launcher := &recordingLauncher{called: make(chan string, 1)}
	starter := &blockingCodexStarter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	application := New(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		launcher,
		platform.FileLauncher{},
		starter,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- application.Run(ctx, root)
	}()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if stopped {
			return
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("application did not shut down during cleanup")
		}
	})

	var targetURL string
	select {
	case targetURL = <-launcher.called:
	case <-time.After(10 * time.Second):
		t.Fatal("application did not reach browser launch")
	}
	select {
	case <-starter.started:
	case <-time.After(10 * time.Second):
		t.Fatal("application did not reach Codex startup")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(targetURL + "assets")
	if err != nil {
		t.Fatal(err)
	}
	closeHTTPResponse(t, response)
	var cookie *http.Cookie
	for _, candidate := range response.Cookies() {
		if candidate.Name == "alm_csrf" {
			cookie = candidate
			break
		}
	}
	if cookie == nil {
		t.Fatal("initial response did not set request-protection cookie")
	}
	token, _, ok := strings.Cut(cookie.Value, ".")
	if !ok || token == "" {
		t.Fatalf("request-protection cookie = %q", cookie.Value)
	}

	close(starter.release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err = client.Get(targetURL + "processing")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			closeHTTPResponse(t, response)
			if readErr == nil && strings.Contains(string(body), "Startup scan completed.") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("application did not install the final HTTP handler")
		}
		time.Sleep(10 * time.Millisecond)
	}

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		targetURL+"assets/00000000000000000000000000000000/open",
		strings.NewReader("csrf_token="+token),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", strings.TrimSuffix(targetURL, "/"))
	request.AddCookie(cookie)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeHTTPResponse(t, response)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("POST after handler update = %d, want %d", response.StatusCode, http.StatusNotFound)
	}

	cancel()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("application did not shut down")
	}
}

func TestRunCleansPreviousStagingEntries(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Server.Port = availablePort(t)
	writeApplicationConfig(t, root, cfg)
	paths, err := storage.Prepare(root, cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sqlite.Open(context.Background(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{
		filepath.Join(paths.Staging, "previous-run.txt"),
		filepath.Join(paths.Staging, "11111111111111111111111111111111.scratch", "analysis.png"),
		filepath.Join(paths.AnalysisWorkspace, "previous-run", "artifact.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(entry, []byte("leftover"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	launcher := &recordingLauncher{called: make(chan string, 1)}
	application := New(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		launcher,
		platform.FileLauncher{},
		staticCodexStarter{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- application.Run(ctx, root)
	}()
	select {
	case <-launcher.called:
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
	entries, err := os.ReadDir(paths.Staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".codex-analysis" {
		t.Fatalf("staging entries = %v, want empty analysis workspace only", entries)
	}
	workspaceEntries, err := os.ReadDir(paths.AnalysisWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaceEntries) != 0 {
		t.Fatalf("analysis workspace entries = %v, want empty", workspaceEntries)
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
		platform.FileLauncher{},
		staticCodexStarter{},
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

type staticCodexStarter struct {
	err error
}

type blockingCodexStarter struct {
	started chan struct{}
	release chan struct{}
}

func (c staticCodexStarter) Start(
	context.Context,
	codex.AnalyzerConfig,
	codex.AttemptRecorder,
) (codex.Runtime, error) {
	return nil, c.err
}

func (starter *blockingCodexStarter) Start(
	ctx context.Context,
	_ codex.AnalyzerConfig,
	_ codex.AttemptRecorder,
) (codex.Runtime, error) {
	close(starter.started)
	select {
	case <-starter.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

func closeHTTPResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Fatalf("closing HTTP response: %v", err)
	}
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
