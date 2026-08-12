// Package web serves the loopback-only HTML application.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/dfcut8/assets-library-manager/internal/codex"
)

//go:embed templates/*.html static/*.css
var assets embed.FS

// Status contains safe startup information rendered by the bootstrap catalog page.
type Status struct {
	CodexState codex.State
	CodexPlan  string
	Database   string
	Incoming   string
	Processed  string
}

// New builds the HTTP handler with strict Host validation and security headers.
func New(allowedHost string, status Status) (http.Handler, error) {
	page, err := template.ParseFS(assets, "templates/assets.html")
	if err != nil {
		return nil, fmt.Errorf("parsing web templates: %w", err)
	}
	staticFiles, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("loading static assets: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/assets", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /assets", func(w http.ResponseWriter, _ *http.Request) {
		var body bytes.Buffer
		if err := page.ExecuteTemplate(&body, "assets.html", status); err != nil {
			http.Error(w, "Unable to render the catalog status.", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if _, err := body.WriteTo(w); err != nil {
			return
		}
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))

	return securityHeaders(allowedHost, mux), nil
}

func securityHeaders(allowedHost string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != allowedHost {
			http.Error(w, "Unrecognized local host.", http.StatusMisdirectedRequest)
			return
		}

		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
