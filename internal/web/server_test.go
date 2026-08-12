package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dfcut8/assets-library-manager/internal/codex"
)

func TestHandlerRedirectsAndRendersReadyStatus(t *testing.T) {
	handler, err := New("127.0.0.1:7342", Status{
		CodexState: codex.StateReady,
		CodexPlan:  "plus",
		Database:   "assets.db",
		Incoming:   "incoming",
		Processed:  "processed",
	})
	if err != nil {
		t.Fatal(err)
	}

	redirect := request(t, handler, "/", "127.0.0.1:7342")
	if redirect.Code != http.StatusSeeOther || redirect.Header().Get("Location") != "/assets" {
		t.Fatalf("GET / = %d location %q", redirect.Code, redirect.Header().Get("Location"))
	}

	page := request(t, handler, "/assets", "127.0.0.1:7342")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /assets = %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "Codex subscription ready") ||
		!strings.Contains(page.Body.String(), "plus plan") {
		t.Fatalf("status page = %q, want ready subscription banner", page.Body.String())
	}
	for _, header := range []string{
		"Content-Security-Policy",
		"Referrer-Policy",
		"X-Content-Type-Options",
		"X-Frame-Options",
	} {
		if page.Header().Get(header) == "" {
			t.Fatalf("security header %s is missing", header)
		}
	}
}

func TestHandlerRendersCodexGuidance(t *testing.T) {
	tests := []struct {
		name  string
		state codex.State
		want  string
	}{
		{name: "signed out", state: codex.StateSignedOut, want: "Codex sign-in required"},
		{name: "api key", state: codex.StateAPIKey, want: "Codex is using an API key"},
		{name: "unsupported", state: codex.StateUnsupported, want: "Unsupported Codex account"},
		{name: "unavailable", state: codex.StateUnavailable, want: "Codex unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, err := New("127.0.0.1:7342", Status{CodexState: tt.state})
			if err != nil {
				t.Fatal(err)
			}
			page := request(t, handler, "/assets", "127.0.0.1:7342")
			if !strings.Contains(page.Body.String(), tt.want) {
				t.Fatalf("status page = %q, want %q", page.Body.String(), tt.want)
			}
		})
	}
}

func TestHandlerRejectsUnrecognizedHost(t *testing.T) {
	handler, err := New("127.0.0.1:7342", Status{})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, "/assets", "localhost:7342")
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unrecognized Host response = %d, want %d", response.Code, http.StatusMisdirectedRequest)
	}
}

func request(t *testing.T, handler http.Handler, path, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	req.Host = host
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	return response
}
