// Package web serves the loopback-only HTML application.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/codex"
	"github.com/dfcut8/assets-library-manager/internal/importer"
)

const (
	metadataBodyLimit = 64 << 10
	revealBodyLimit   = 8 << 10
	requestTimeout    = 10 * time.Second
)

//go:embed templates/*.html static/*
var assets embed.FS

// Status contains safe startup information rendered by the catalog page.
type Status struct {
	CodexState codex.State
	CodexPlan  string
	Database   string
	Incoming   string
	Processed  string
}

// CatalogService is the narrow catalog use case consumed by HTTP handlers.
type CatalogService interface {
	Search(ctx context.Context, query catalog.AssetQuery) (catalog.Page[catalog.AssetSummary], error)
	Get(ctx context.Context, id catalog.AssetID) (catalog.AssetDetail, error)
	GetThumbnail(ctx context.Context, id catalog.AssetID) (catalog.Thumbnail, error)
	GetOriginal(ctx context.Context, id catalog.AssetID) (catalog.Original, error)
	UpdateSemanticMetadata(ctx context.Context, id catalog.AssetID, edit catalog.MetadataEdit) (catalog.AssetDetail, error)
}

// ProcessingReader supplies immutable progress snapshots while the startup scan runs.
type ProcessingReader interface {
	Snapshot() importer.Progress
}

// ManagedOpener opens a validated managed original beneath processed/.
type ManagedOpener interface {
	OpenManaged(importer.ManagedPath) (*os.File, error)
}

// FileRevealer opens one trusted managed file in the platform file manager.
type FileRevealer interface {
	Reveal(context.Context, string) error
}

// Dependencies are the explicit, testable boundaries used by the HTML server.
type Dependencies struct {
	Status     Status
	Catalog    CatalogService
	Processing ProcessingReader
	Managed    ManagedOpener
	Revealer   FileRevealer
	CSRFSecret []byte
}

// New builds the full HTTP handler with strict Host, origin, CSRF, and security-header checks.
func New(allowedHost string, dependencies Dependencies) (http.Handler, error) {
	if allowedHost == "" {
		return nil, errors.New("creating web server: allowed host is empty")
	}
	if dependencies.Catalog == nil || dependencies.Managed == nil || dependencies.Revealer == nil {
		return nil, errors.New("creating web server: dependencies are incomplete")
	}
	page, err := template.New("pages").Funcs(template.FuncMap{
		"groupTags": groupTags,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing web templates: %w", err)
	}
	staticFiles, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("loading static assets: %w", err)
	}
	secret := append([]byte(nil), dependencies.CSRFSecret...)
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("creating csrf secret: %w", err)
		}
	}
	if len(secret) < 32 {
		return nil, errors.New("creating web server: csrf secret is too short")
	}

	server := &server{
		allowedHost: allowedHost, dependencies: dependencies, templates: page,
		csrf: newCSRF(secret),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/assets", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /assets", server.catalog)
	mux.HandleFunc("GET /assets/fragment", server.catalogFragment)
	mux.HandleFunc("GET /assets/{id}", server.detail)
	mux.HandleFunc("GET /assets/{id}/thumbnail", server.thumbnail)
	mux.HandleFunc("GET /assets/{id}/download", server.download)
	mux.HandleFunc("POST /assets/{id}/metadata", server.metadata)
	mux.HandleFunc("POST /assets/{id}/reveal", server.reveal)
	mux.HandleFunc("GET /processing", server.processing)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))

	return securityHeaders(allowedHost, server.csrf.protect(mux)), nil
}

func securityHeaders(allowedHost string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != allowedHost {
			http.Error(w, "Unrecognized local host.", http.StatusMisdirectedRequest)
			return
		}

		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
