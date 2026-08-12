// Package app composes startup, lifecycle, and shutdown behavior.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/codex"
	"github.com/dfcut8/assets-library-manager/internal/config"
	"github.com/dfcut8/assets-library-manager/internal/sqlite"
	"github.com/dfcut8/assets-library-manager/internal/storage"
	"github.com/dfcut8/assets-library-manager/internal/web"
)

// URLLauncher opens the trusted local application URL.
type URLLauncher interface {
	Open(ctx context.Context, targetURL string) error
}

// CodexChecker verifies that subscription-backed processing is available.
type CodexChecker interface {
	Check(ctx context.Context, command string) (codex.Status, error)
}

// Application owns the process lifecycle and its injected platform boundary.
type Application struct {
	logger   *slog.Logger
	launcher URLLauncher
	codex    CodexChecker
}

// New constructs an application with explicit process-level dependencies.
func New(logger *slog.Logger, launcher URLLauncher, codexChecker CodexChecker) *Application {
	return &Application{
		logger:   logger,
		launcher: launcher,
		codex:    codexChecker,
	}
}

// ExecutableRoot returns the canonical directory containing the running executable.
func ExecutableRoot() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("canonicalizing executable: %w", err)
	}

	return filepath.Dir(executable), nil
}

// Run initializes runtime state and serves until cancellation or a server failure.
func (a *Application) Run(ctx context.Context, root string) error {
	cfg, wasCreated, err := config.LoadOrCreate(root)
	if err != nil {
		return err
	}
	if wasCreated {
		a.logger.Info("created default configuration", "file", config.FileName)
	}

	paths, err := storage.Prepare(root, cfg.Storage)
	if err != nil {
		return err
	}
	database, err := sqlite.Open(ctx, paths.Database)
	if err != nil {
		return err
	}

	codexCtx, cancelCodex := context.WithTimeout(
		ctx,
		time.Duration(cfg.Codex.StartupTimeoutSeconds)*time.Second,
	)
	codexStatus, codexErr := a.codex.Check(codexCtx, cfg.Codex.Command)
	cancelCodex()
	if codexErr != nil {
		a.logger.Warn("codex preflight failed", "error", codexErr)
	}

	runErr := a.serve(ctx, cfg, codexStatus)
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		time.Duration(cfg.Server.ShutdownTimeoutSeconds)*time.Second,
	)
	defer cancel()
	checkpointErr := database.Checkpoint(cleanupCtx)
	closeErr := database.Close()

	return errors.Join(runErr, checkpointErr, closeErr)
}

func (a *Application) serve(ctx context.Context, cfg config.Config, codexStatus codex.Status) error {
	address := net.JoinHostPort(cfg.Server.Host, fmt.Sprintf("%d", cfg.Server.Port))
	handler, err := web.New(address, web.Status{
		CodexState: codexStatus.State,
		CodexPlan:  codexStatus.PlanType,
		Database:   cfg.Storage.Database,
		Incoming:   cfg.Storage.IncomingDirectory,
		Processed:  cfg.Storage.ProcessedDirectory,
	})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on loopback address: %w", err)
	}

	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	localURL := "http://" + address + "/"
	a.logger.Info("local server ready", "url", localURL)
	if cfg.Server.OpenBrowser {
		browserCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := a.launcher.Open(browserCtx, localURL); err != nil {
			a.logger.Warn("browser launch failed", "url", localURL, "error", err)
		}
		cancel()
	}

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving local application: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		time.Duration(cfg.Server.ShutdownTimeoutSeconds)*time.Second,
	)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		closeErr := server.Close()
		if closeErr != nil {
			return errors.Join(
				fmt.Errorf("shutting down local server: %w", err),
				fmt.Errorf("closing local server: %w", closeErr),
			)
		}
		return fmt.Errorf("shutting down local server: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("joining local server: %w", err)
	}

	return nil
}
