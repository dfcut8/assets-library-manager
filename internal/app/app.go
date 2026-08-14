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
	"sync"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/codex"
	"github.com/dfcut8/assets-library-manager/internal/config"
	"github.com/dfcut8/assets-library-manager/internal/imageinspect"
	"github.com/dfcut8/assets-library-manager/internal/importer"
	"github.com/dfcut8/assets-library-manager/internal/sqlite"
	"github.com/dfcut8/assets-library-manager/internal/storage"
	"github.com/dfcut8/assets-library-manager/internal/web"
)

// URLLauncher opens the trusted local application URL.
type URLLauncher interface {
	Open(ctx context.Context, targetURL string) error
}

// FileLauncher opens a trusted managed original in its viewer or file manager.
type FileLauncher interface {
	Open(context.Context, string) error
	Reveal(context.Context, string) error
}

// AnalyzerStarter creates one owned production semantic analyzer.
type AnalyzerStarter interface {
	Start(context.Context, codex.AnalyzerConfig, codex.AttemptRecorder) (codex.Runtime, error)
}

// Application owns the process lifecycle and its injected platform boundaries.
type Application struct {
	logger   *slog.Logger
	launcher URLLauncher
	files    FileLauncher
	codex    AnalyzerStarter
}

// New constructs an application with explicit process-level dependencies.
func New(logger *slog.Logger, launcher URLLauncher, files FileLauncher, starter AnalyzerStarter) *Application {
	return &Application{logger: logger, launcher: launcher, files: files, codex: starter}
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

// Run recovers runtime state, starts HTTP, launches one startup scan, and shuts down in order.
func (a *Application) Run(ctx context.Context, root string) (returnErr error) {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	paths, err := storage.Prepare(root, cfg.Storage)
	if err != nil {
		return err
	}
	database, err := sqlite.Open(ctx, paths.Database)
	if err != nil {
		return err
	}
	store, err := storage.OpenStore(paths)
	if err != nil {
		return errors.Join(err, database.Close())
	}
	resourcesOpen := true
	defer func() {
		if resourcesOpen {
			returnErr = errors.Join(returnErr, store.Close(), database.Close())
		}
	}()

	coordinatorConfig := processingConfig(cfg)
	recovery, err := importer.NewCoordinator(
		coordinatorConfig, database, store, imageinspect.New(), nil, a.logger,
	)
	if err != nil {
		return err
	}
	if err := recovery.Recover(ctx); err != nil {
		return fmt.Errorf("recovering imports: %w", err)
	}
	catalogService, err := catalog.NewService(database, database, database, database)
	if err != nil {
		return err
	}
	webStatus := web.Status{CodexState: codex.StateUnavailable,
		Database: cfg.Storage.Database, Incoming: cfg.Storage.IncomingDirectory,
		Processed: cfg.Storage.ProcessedDirectory}

	address := net.JoinHostPort(cfg.Server.Host, fmt.Sprintf("%d", cfg.Server.Port))
	handler, err := web.New(address, web.Dependencies{
		Status:  webStatus,
		Catalog: catalogService, Processing: recovery, Managed: store, Files: a.files,
	})
	if err != nil {
		return err
	}
	handlers := &handlerSwitcher{handler: handler}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on loopback address: %w", err)
	}
	server := newHTTPServer(address, handlers)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	localURL := "http://" + address + "/"
	a.logger.Info("local server ready", "url", localURL)
	if cfg.Server.OpenBrowser {
		browserCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := a.launcher.Open(browserCtx, localURL); err != nil {
			a.logger.Warn("browser launch failed", "url", localURL, "error", err)
		}
		cancel()
	}

	var analyzer codex.Runtime
	startupCtx, cancelStartup := context.WithTimeout(
		ctx, time.Duration(cfg.Codex.StartupTimeoutSeconds)*time.Second,
	)
	analyzer, analyzerErr := a.codex.Start(
		startupCtx, analyzerConfig(cfg, paths.AnalysisWorkspace, a.logger), database,
	)
	cancelStartup()
	if analyzerErr != nil || analyzer == nil {
		if analyzerErr == nil {
			analyzerErr = errors.New("codex analyzer starter returned no runtime")
		}
		a.logger.Warn("codex analyzer unavailable; new imports will be retained", "error", analyzerErr)
		analyzer = nil
	} else {
		status := analyzer.Status()
		webStatus.CodexState = status.State
		webStatus.CodexPlan = status.PlanType
		updatedHandler, handlerErr := web.New(address, web.Dependencies{
			Status:  webStatus,
			Catalog: catalogService, Processing: recovery, Managed: store, Files: a.files,
		})
		if handlerErr != nil {
			return errors.Join(
				handlerErr,
				shutdownServer(server, serveErr, cfg.Server.ShutdownTimeoutSeconds),
				analyzer.Close(),
			)
		}
		handlers.Store(updatedHandler)
	}

	coordinator, err := importer.NewCoordinator(
		coordinatorConfig, database, store, imageinspect.New(), analyzer, a.logger,
	)
	if err != nil {
		var analyzerCloseErr error
		if analyzer != nil {
			analyzerCloseErr = analyzer.Close()
		}
		return errors.Join(
			err,
			shutdownServer(server, serveErr, cfg.Server.ShutdownTimeoutSeconds),
			analyzerCloseErr,
		)
	}
	updatedHandler, handlerErr := web.New(address, web.Dependencies{
		Status:  webStatus,
		Catalog: catalogService, Processing: coordinator, Managed: store, Files: a.files,
	})
	if handlerErr != nil {
		var analyzerCloseErr error
		if analyzer != nil {
			analyzerCloseErr = analyzer.Close()
		}
		return errors.Join(handlerErr, shutdownServer(server, serveErr, cfg.Server.ShutdownTimeoutSeconds), analyzerCloseErr)
	}
	handlers.Store(updatedHandler)
	processingCtx, cancelProcessing := context.WithCancel(ctx)
	processingDone := make(chan error, 1)
	go func() { processingDone <- coordinator.Run(processingCtx) }()

	processingJoined, runErr := waitForShutdown(ctx, serveErr, processingDone, a.logger)
	coordinator.StopReservations()
	serverErr := shutdownServer(server, serveErr, cfg.Server.ShutdownTimeoutSeconds)
	cancelProcessing()
	var processingErr error
	if !processingJoined {
		processingErr = joinProcessing(processingDone)
	}
	var analyzerCloseErr error
	if analyzer != nil {
		analyzerCloseErr = analyzer.Close()
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.WithoutCancel(ctx),
		time.Duration(cfg.Server.ShutdownTimeoutSeconds)*time.Second,
	)
	checkpointErr := database.Checkpoint(cleanupCtx)
	cancelCleanup()
	storeCloseErr := store.Close()
	databaseCloseErr := database.Close()
	resourcesOpen = false

	return errors.Join(
		runErr, serverErr, processingErr, analyzerCloseErr,
		checkpointErr, storeCloseErr, databaseCloseErr,
	)
}

func processingConfig(cfg config.Config) importer.CoordinatorConfig {
	return importer.CoordinatorConfig{
		Workers: cfg.Processing.Workers, MaxSourceBytes: cfg.Processing.MaxSourceBytes,
		InspectionLimits: imageinspect.Limits{
			MaxSourceBytes:        cfg.Processing.MaxSourceBytes,
			MaxImagePixels:        cfg.Processing.MaxImagePixels,
			ThumbnailMaxDimension: cfg.Processing.ThumbnailMaxDimension,
			AnalysisMaxDimension:  cfg.Processing.AnalysisMaxDimension,
			MaxAnalysisBytes:      cfg.Processing.MaxAnalysisBytes,
		},
		ArchiveLimits: importer.ArchiveLimits{
			MaxEntries:                cfg.Processing.Archive.MaxEntries,
			MaxEntryBytes:             cfg.Processing.Archive.MaxEntryBytes,
			MaxTotalUncompressedBytes: cfg.Processing.Archive.MaxTotalUncompressedBytes,
			MaxCompressionRatio:       cfg.Processing.Archive.MaxCompressionRatio,
		},
	}
}

func analyzerConfig(cfg config.Config, workingDirectory string, logger *slog.Logger) codex.AnalyzerConfig {
	return codex.AnalyzerConfig{
		Command: cfg.Codex.Command, Model: cfg.Codex.Model,
		WorkingDirectory:  workingDirectory,
		ReasoningEffort:   cfg.Codex.ReasoningEffort,
		TurnTimeout:       time.Duration(cfg.Codex.TurnTimeoutSeconds) * time.Second,
		MaxAttempts:       cfg.Codex.MaxAttempts,
		InitialRetryDelay: time.Duration(cfg.Codex.InitialRetryDelayMS) * time.Millisecond,
		Logger:            logger,
	}
}

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
}

func waitForShutdown(
	ctx context.Context,
	serveErr <-chan error,
	processingDone <-chan error,
	logger *slog.Logger,
) (bool, error) {
	processingJoined := false
	for {
		select {
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return processingJoined, nil
			}

			return processingJoined, fmt.Errorf("serving local application: %w", err)
		case err := <-processingDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("startup import scan failed", "error", err)
			}
			processingJoined = true
			processingDone = nil
		case <-ctx.Done():
			return processingJoined, nil
		}
	}
}

func shutdownServer(server *http.Server, serveErr <-chan error, timeoutSeconds int) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return errors.Join(
			fmt.Errorf("shutting down local server: %w", err),
			wrapCloseError(server.Close()),
		)
	}
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("joining local server: %w", err)
		}
	default:
	}

	return nil
}

func joinProcessing(done <-chan error) error {
	if done == nil {
		return nil
	}
	err := <-done
	if errors.Is(err, context.Canceled) {
		return nil
	}

	return err
}

func wrapCloseError(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("closing local server: %w", err)
}

type handlerSwitcher struct {
	mu      sync.RWMutex
	handler http.Handler
}

func (switcher *handlerSwitcher) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switcher.mu.RLock()
	handler := switcher.handler
	switcher.mu.RUnlock()
	handler.ServeHTTP(writer, request)
}

func (switcher *handlerSwitcher) Store(handler http.Handler) {
	switcher.mu.Lock()
	switcher.handler = handler
	switcher.mu.Unlock()
}
