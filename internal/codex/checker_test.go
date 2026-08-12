package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestChecker_Check(t *testing.T) {
	tests := []struct {
		name       string
		account    any
		wantState  State
		wantPlan   string
		wantMethod string
	}{
		{
			name:      "chatgpt subscription",
			account:   map[string]any{"type": "chatgpt", "planType": "plus"},
			wantState: StateReady,
			wantPlan:  "plus",
		},
		{
			name:      "signed out",
			account:   nil,
			wantState: StateSignedOut,
		},
		{
			name:      "api key authentication",
			account:   map[string]any{"type": "apiKey"},
			wantState: StateAPIKey,
		},
		{
			name:      "unsupported authentication",
			account:   map[string]any{"type": "amazonBedrock"},
			wantState: StateUnsupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker, received := fakeChecker(t, tt.account)
			status, err := checker.Check(t.Context(), "codex-test")
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if status.State != tt.wantState || status.PlanType != tt.wantPlan {
				t.Fatalf("Check() = %#v, want state %q plan %q", status, tt.wantState, tt.wantPlan)
			}
			if got := <-received; got != "initialize,initialized,account/read" {
				t.Fatalf("App Server methods = %q", got)
			}
		})
	}
}

func TestChecker_CheckReportsStartAndProtocolFailures(t *testing.T) {
	tests := []struct {
		name    string
		checker *Checker
	}{
		{
			name: "start failure",
			checker: &Checker{start: func(context.Context, string) (*process, error) {
				return nil, errors.New("not installed")
			}},
		},
		{
			name: "protocol failure",
			checker: &Checker{start: func(context.Context, string) (*process, error) {
				return &process{
					stdin:  writeCloser{Writer: io.Discard},
					stdout: io.NopCloser(strings.NewReader("not-json\n")),
					wait:   func() error { return nil },
				}, nil
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := tt.checker.Check(t.Context(), "codex-test")
			if err == nil {
				t.Fatal("Check() error = nil")
			}
			if status.State != StateUnavailable {
				t.Fatalf("Check() state = %q, want %q", status.State, StateUnavailable)
			}
		})
	}
}

type writeCloser struct {
	io.Writer
}

func (writeCloser) Close() error {
	return nil
}

func fakeChecker(t *testing.T, account any) (*Checker, <-chan string) {
	t.Helper()
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	received := make(chan string, 1)

	go func() {
		decoder := json.NewDecoder(serverReader)
		encoder := json.NewEncoder(serverWriter)
		methods := make([]string, 0, 3)
		for range 3 {
			var request struct {
				Method string `json:"method"`
				ID     *int   `json:"id"`
			}
			if err := decoder.Decode(&request); err != nil {
				received <- "decode error: " + err.Error()
				return
			}
			methods = append(methods, request.Method)
			if request.ID == nil {
				continue
			}

			result := map[string]any{"serverInfo": map[string]string{"name": "fake"}}
			if request.Method == "account/read" {
				result = map[string]any{"account": account, "requiresOpenaiAuth": true}
			}
			if err := encoder.Encode(map[string]any{"id": *request.ID, "result": result}); err != nil {
				received <- "encode error: " + err.Error()
				return
			}
		}
		received <- strings.Join(methods, ",")
	}()

	return &Checker{start: func(context.Context, string) (*process, error) {
		return &process{
			stdin:  clientWriter,
			stdout: clientReader,
			wait: func() error {
				if err := serverReader.Close(); err != nil {
					return err
				}

				return serverWriter.Close()
			},
		}, nil
	}}, received
}
