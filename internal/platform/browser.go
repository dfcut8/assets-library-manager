// Package platform provides direct, shell-free operating-system integrations.
package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
)

// FileLauncher opens trusted managed files with native operating-system integrations.
type FileLauncher struct{}

// Reveal invokes the platform file revealer without using a shell.
func (FileLauncher) Reveal(ctx context.Context, targetPath string) error {
	name, args, err := revealCommand(runtime.GOOS, targetPath)
	if err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("revealing file: %w", err)
	}

	return nil
}

// Open invokes the file's default viewer without using a shell.
func (FileLauncher) Open(ctx context.Context, targetPath string) error {
	name, args, err := viewerCommand(runtime.GOOS, targetPath)
	if err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("opening file: %w", err)
	}

	return nil
}

// Browser opens trusted loopback URLs with the operating system's default browser.
type Browser struct{}

// Open invokes the platform browser launcher without using a shell.
func (Browser) Open(ctx context.Context, targetURL string) error {
	name, args, err := browserCommand(runtime.GOOS, targetURL)
	if err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("opening browser: %w", err)
	}

	return nil
}

func browserCommand(goos, targetURL string) (string, []string, error) {
	return defaultOpenCommand(goos, targetURL)
}

func viewerCommand(goos, targetPath string) (string, []string, error) {
	return defaultOpenCommand(goos, targetPath)
}

func defaultOpenCommand(goos, target string) (string, []string, error) {
	switch goos {
	case "windows":
		return "rundll32.exe", []string{"url.dll,FileProtocolHandler", target}, nil
	case "darwin":
		return "open", []string{target}, nil
	case "linux":
		return "xdg-open", []string{target}, nil
	default:
		return "", nil, errors.New("platform: default opening is unsupported")
	}
}

func revealCommand(goos, targetPath string) (string, []string, error) {
	switch goos {
	case "windows":
		return "explorer.exe", []string{"/select,", targetPath}, nil
	case "darwin":
		return "open", []string{"-R", targetPath}, nil
	case "linux":
		return "xdg-open", []string{filepath.Dir(targetPath)}, nil
	default:
		return "", nil, errors.New("platform: file revealing is unsupported")
	}
}
