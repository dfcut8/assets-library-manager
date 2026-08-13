// Command asset-library-manager runs the local Asset Library Manager web application.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/dfcut8/assets-library-manager/internal/app"
	"github.com/dfcut8/assets-library-manager/internal/codex"
	"github.com/dfcut8/assets-library-manager/internal/platform"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("asset-library-manager", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version information and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		if _, err := fmt.Fprintln(stderr, "asset-library-manager does not accept positional arguments"); err != nil {
			return 1
		}
		return 2
	}
	if *showVersion {
		if _, err := fmt.Fprintf(
			stdout,
			"asset-library-manager %s (commit %s, %s)\n",
			version,
			commit,
			runtime.Version(),
		); err != nil {
			return 1
		}
		return 0
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	root, err := app.ExecutableRoot()
	if err != nil {
		logger.Error("startup failed", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application := app.New(logger, platform.Browser{}, platform.Revealer{}, codex.NewStarter())
	if err := application.Run(ctx, root); err != nil {
		logger.Error("application stopped with an error", "error", err)
		return 1
	}

	return 0
}
