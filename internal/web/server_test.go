package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerRedirectsAndRendersStatus(t *testing.T) {
	handler, err := New("127.0.0.1:7342", Status{
		Database:  "assets.db",
		Incoming:  "incoming",
		Processed: "processed",
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
	if !strings.Contains(page.Body.String(), "OpenAI not configured") {
		t.Fatalf("status page = %q, want missing-key banner", page.Body.String())
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
