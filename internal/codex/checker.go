// Package codex connects the application to a host-provided Codex App Server.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

const (
	maxMessageBytes = 1 << 20
	maxMessages     = 64
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

type startFunc func(context.Context, string) (*process, error)

type process struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	wait   func() error
}

type response struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *responseError  `json:"error"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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
func (c *Checker) Check(ctx context.Context, command string) (status Status, returnErr error) {
	processCtx, cancel := context.WithCancel(ctx)
	proc, err := c.start(processCtx, command)
	if err != nil {
		cancel()

		return Status{State: StateUnavailable}, err
	}
	defer func() {
		cancel()
		if err := proc.close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing codex app server: %w", err))
		}
	}()

	encoder := json.NewEncoder(proc.stdin)
	scanner := bufio.NewScanner(proc.stdout)
	scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)

	if err := encoder.Encode(map[string]any{
		"method": "initialize",
		"id":     1,
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "asset_library_manager",
				"title":   "Asset Library Manager",
				"version": "0.1.0",
			},
		},
	}); err != nil {
		return Status{State: StateUnavailable}, fmt.Errorf("initializing codex app server: %w", err)
	}
	if _, err := readResponse(scanner, 1); err != nil {
		return Status{State: StateUnavailable}, fmt.Errorf("initializing codex app server: %w", err)
	}
	if err := encoder.Encode(map[string]any{
		"method": "initialized",
		"params": map[string]any{},
	}); err != nil {
		return Status{State: StateUnavailable}, fmt.Errorf("acknowledging codex app server: %w", err)
	}
	if err := encoder.Encode(map[string]any{
		"method": "account/read",
		"id":     2,
		"params": map[string]bool{"refreshToken": false},
	}); err != nil {
		return Status{State: StateUnavailable}, fmt.Errorf("requesting codex account: %w", err)
	}

	rawResult, err := readResponse(scanner, 2)
	if err != nil {
		return Status{State: StateUnavailable}, fmt.Errorf("reading codex account: %w", err)
	}
	var result accountResult
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return Status{State: StateUnavailable}, fmt.Errorf("decoding codex account: %w", err)
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

func startCommand(ctx context.Context, command string) (*process, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("locating codex command %q: %w", command, err)
	}

	cmd := exec.CommandContext(ctx, path, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("opening codex stdout: %w", err),
				fmt.Errorf("closing codex stdin: %w", closeErr),
			)
		}

		return nil, fmt.Errorf("opening codex stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("starting codex app server: %w", err),
			closePipe("codex stdin", stdin),
			closePipe("codex stdout", stdout),
		)
	}

	return &process{
		stdin:  stdin,
		stdout: stdout,
		wait:   cmd.Wait,
	}, nil
}

func readResponse(scanner *bufio.Scanner, id int) (json.RawMessage, error) {
	for range maxMessages {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("reading app server message: %w", err)
			}

			return nil, io.ErrUnexpectedEOF
		}

		var message response
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return nil, fmt.Errorf("decoding app server message: %w", err)
		}
		if message.ID == nil || *message.ID != id {
			continue
		}
		if message.Error != nil {
			return nil, fmt.Errorf(
				"app server error %d: %s",
				message.Error.Code,
				message.Error.Message,
			)
		}
		if len(message.Result) == 0 {
			return nil, errors.New("app server response has no result")
		}

		return message.Result, nil
	}

	return nil, fmt.Errorf("app server response %d not received within %d messages", id, maxMessages)
}

func (p *process) close() error {
	stdinErr := closePipe("codex stdin", p.stdin)
	stdoutErr := closePipe("codex stdout", p.stdout)
	waitErr := p.wait()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		waitErr = nil
	}

	return errors.Join(stdinErr, stdoutErr, waitErr)
}

func closePipe(name string, closer io.Closer) error {
	if err := closer.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", name, err)
	}

	return nil
}
