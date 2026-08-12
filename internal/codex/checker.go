// Package codex connects the application to a host-provided Codex App Server.
package codex

import (
	"context"
	"errors"
	"fmt"
)

// State describes whether subscription-backed processing can start.
type State string

const (
	// StateReady means Codex is authenticated with a ChatGPT account.
	StateReady State = "ready"
	// StateSignedOut means Codex requires a login before processing can start.
	StateSignedOut State = "signed_out"
	// StateAPIKey means Codex is using usage-based API authentication instead of ChatGPT.
	StateAPIKey State = "api_key"
	// StateUnsupported means Codex is authenticated with a provider other than ChatGPT.
	StateUnsupported State = "unsupported"
	// StateUnavailable means the command could not complete the App Server handshake.
	StateUnavailable State = "unavailable"
)

// Status contains safe account information returned by the startup preflight.
type Status struct {
	State    State
	PlanType string
}

// Checker starts a short-lived App Server process and verifies its account state.
type Checker struct {
	start startFunc
}

type accountResult struct {
	Account *account `json:"account"`
}

type account struct {
	Type     string `json:"type"`
	PlanType string `json:"planType"`
}

// New constructs a Checker that resolves and starts the configured Codex command.
func New() *Checker {
	return &Checker{start: startCommand}
}

// Check verifies that the command speaks the App Server protocol and uses ChatGPT authentication.
func (checker *Checker) Check(ctx context.Context, command string) (status Status, returnErr error) {
	client, err := startTransport(ctx, command, checker.start)
	if err != nil {
		return Status{State: StateUnavailable}, err
	}
	defer func() {
		if err := client.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing codex app server: %w", err))
		}
	}()

	var result accountResult
	if err := client.request(ctx, "account/read", map[string]bool{"refreshToken": false}, &result); err != nil {
		return Status{State: StateUnavailable}, fmt.Errorf("reading codex account: %w", err)
	}
	if result.Account == nil {
		return Status{State: StateSignedOut}, nil
	}
	switch result.Account.Type {
	case "chatgpt":
		return Status{State: StateReady, PlanType: result.Account.PlanType}, nil
	case "apiKey":
		return Status{State: StateAPIKey}, nil
	default:
		return Status{State: StateUnsupported}, nil
	}
}
