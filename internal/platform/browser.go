// Package platform provides direct, shell-free operating-system integrations.
package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

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
	switch goos {
	case "windows":
		return "rundll32.exe", []string{"url.dll,FileProtocolHandler", targetURL}, nil
	case "darwin":
		return "open", []string{targetURL}, nil
	case "linux":
		return "xdg-open", []string{targetURL}, nil
	default:
		return "", nil, errors.New("platform: browser launching is unsupported")
	}
}
