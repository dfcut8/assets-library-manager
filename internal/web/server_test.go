package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/codex"
	"github.com/dfcut8/assets-library-manager/internal/importer"
)

const testAssetID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestCatalogRoutesRenderAndRespectHTMX(t *testing.T) {
	handler, _ := newTestHandler(t)
	redirect := request(t, handler, http.MethodGet, "/", "127.0.0.1:7342", nil)
	if redirect.Code != http.StatusSeeOther || redirect.Header().Get("Location") != "/assets" {
		t.Fatalf("GET / = %d location %q", redirect.Code, redirect.Header().Get("Location"))
	}
	page := request(t, handler, http.MethodGet, "/assets?q=blue", "127.0.0.1:7342", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Blue Knight") {
		t.Fatalf("catalog response = %d %q", page.Code, page.Body.String())
	}
	fragmentRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7342/assets/fragment", nil)
	fragmentRequest.Host = "127.0.0.1:7342"
	fragmentRequest.Header.Set("HX-Request", "true")
	fragment := httptest.NewRecorder()
	handler.ServeHTTP(fragment, fragmentRequest)
	if fragment.Code != http.StatusOK || !strings.Contains(fragment.Body.String(), "catalog-results") || strings.Contains(fragment.Body.String(), "<!doctype") {
		t.Fatalf("fragment = %d %q", fragment.Code, fragment.Body.String())
	}
	detail := request(t, handler, http.MethodGet, "/assets/"+testAssetID, "127.0.0.1:7342", nil)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "Edit metadata") {
		t.Fatalf("detail = %d %q", detail.Code, detail.Body.String())
	}
	for _, previewMarkup := range []string{
		`data-preview-open`,
		`data-preview-dialog`,
		`src="/static/asset-preview.js"`,
	} {
		if !strings.Contains(detail.Body.String(), previewMarkup) {
			t.Fatalf("detail does not include asset preview markup %q: %q", previewMarkup, detail.Body.String())
		}
	}
}

func TestCatalogControlsUpdateAutomaticallyAndPreserveQuery(t *testing.T) {
	catalogService := &queryRecordingCatalog{}
	handler, _ := newTestHandlerWithCatalog(t, catalogService)
	request := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7342/assets?q=blue&type=character%2Cbuilding&style=Pixel+Art%2CPainted&sort=title",
		nil,
	)
	request.Host = "127.0.0.1:7342"
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("catalog controls response = %d %q", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "<!doctype") || !strings.Contains(response.Body.String(), `id="catalog-results"`) {
		t.Fatalf("catalog controls did not return a results fragment: %q", response.Body.String())
	}
	query := catalogService.query
	if query.Q != "blue" || query.Sort != catalog.SortTitle {
		t.Fatalf("search and sort query = %+v", query)
	}
	if len(query.Types) != 2 || query.Types[0] != catalog.PrimaryTypeCharacter || query.Types[1] != catalog.PrimaryTypeBuilding {
		t.Fatalf("type filters = %+v", query.Types)
	}
	if len(query.Styles) != 2 || query.Styles[0] != "Pixel Art" || query.Styles[1] != "Painted" {
		t.Fatalf("style filters = %+v", query.Styles)
	}

	page := requestCatalogPage(t, handler, "/assets?sort=title")
	if !strings.Contains(page.Body.String(), `<option value="title" selected>Title</option>`) {
		t.Fatalf("selected sort was not preserved: %q", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), `hx-get="/assets"`) || !strings.Contains(page.Body.String(), `hx-swap="outerHTML"`) {
		t.Fatalf("catalog form does not use the canonical URL and outer swap: %q", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), `hx-trigger="input changed delay:300ms from:input[name='q'], change from:select[name='sort']"`) ||
		!strings.Contains(page.Body.String(), `hx-sync="this:replace"`) {
		t.Fatalf("catalog controls do not apply automatically: %q", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), `class="search-filter"`) || !strings.Contains(page.Body.String(), `autofocus`) {
		t.Fatalf("search is not the primary catalog control: %q", page.Body.String())
	}
	for _, removedControl := range []string{`name="type"`, `name="style"`, "Apply filters"} {
		if strings.Contains(page.Body.String(), removedControl) {
			t.Fatalf("catalog page still contains removed control %q: %q", removedControl, page.Body.String())
		}
	}
}

func TestSecurityHeadersAndCSRF(t *testing.T) {
	handler, _ := newTestHandler(t)
	wrongHost := request(t, handler, http.MethodGet, "/assets", "localhost:7342", nil)
	if wrongHost.Code != http.StatusMisdirectedRequest {
		t.Fatalf("wrong host = %d", wrongHost.Code)
	}
	page := request(t, handler, http.MethodGet, "/assets/"+testAssetID, "127.0.0.1:7342", nil)
	for _, header := range []string{"Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if page.Header().Get(header) == "" {
			t.Fatalf("missing %s", header)
		}
	}
	cookie := page.Result().Cookies()[0]
	token := csrfTokenFromHTML(t, page.Body.String())
	missing, _ := postRequest(handler, "/assets/"+testAssetID+"/metadata", "title=x", nil, "http://127.0.0.1:7342")
	if missing.Code != http.StatusForbidden {
		t.Fatalf("missing csrf = %d", missing.Code)
	}
	valid, _ := postRequest(handler, "/assets/"+testAssetID+"/metadata", "csrf_token="+token+"&version=1&title=Blue+Knight&description=A+blue+knight.&primary_type=character&style=Pixel+art&layout_kind=single", cookie, "http://127.0.0.1:7342")
	if valid.Code != http.StatusOK {
		t.Fatalf("valid csrf = %d %q", valid.Code, valid.Body.String())
	}
	badOrigin, _ := postRequest(handler, "/assets/"+testAssetID+"/metadata", "csrf_token="+token, cookie, "http://evil.invalid")
	if badOrigin.Code != http.StatusForbidden {
		t.Fatalf("bad origin = %d", badOrigin.Code)
	}
}

func TestThumbnailDownloadAndReveal(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	thumbnail := request(t, handler, http.MethodGet, "/assets/"+testAssetID+"/thumbnail", "127.0.0.1:7342", nil)
	if thumbnail.Code != http.StatusOK || thumbnail.Header().Get("Content-Type") != "image/png" || !bytes.Equal(thumbnail.Body.Bytes(), []byte("thumbnail")) {
		t.Fatalf("thumbnail = %d %q", thumbnail.Code, thumbnail.Body.Bytes())
	}
	download := request(t, handler, http.MethodGet, "/assets/"+testAssetID+"/download", "127.0.0.1:7342", nil)
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), []byte("original bytes")) || !strings.Contains(download.Header().Get("Content-Disposition"), "asset.png") {
		t.Fatalf("download = %d %q %q", download.Code, download.Body.Bytes(), download.Header().Get("Content-Disposition"))
	}
	page := request(t, handler, http.MethodGet, "/assets/"+testAssetID, "127.0.0.1:7342", nil)
	cookie := page.Result().Cookies()[0]
	token := csrfTokenFromHTML(t, page.Body.String())
	reveal, _ := postRequest(handler, "/assets/"+testAssetID+"/reveal", "csrf_token="+token, cookie, "http://127.0.0.1:7342")
	if reveal.Code != http.StatusSeeOther || dependencies.revealer.path == "" {
		t.Fatalf("reveal = %d %q", reveal.Code, dependencies.revealer.path)
	}
}

func request(t *testing.T, handler http.Handler, method, path, host string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://"+host+path, body)
	req.Host = host
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func postRequest(handler http.Handler, path, body string, cookie *http.Cookie, origin string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7342"+path, strings.NewReader(body))
	req.Host = "127.0.0.1:7342"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response, req
}

func csrfTokenFromHTML(t *testing.T, body string) string {
	t.Helper()
	matches := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
	if len(matches) != 2 {
		t.Fatalf("csrf token missing from %q", body)
	}

	return matches[1]
}

type testDependencies struct {
	revealer *fakeRevealer
}

func newTestHandler(t *testing.T) (http.Handler, testDependencies) {
	t.Helper()

	return newTestHandlerWithCatalog(t, fakeCatalog{})
}

func newTestHandlerWithCatalog(t *testing.T, catalogService CatalogService) (http.Handler, testDependencies) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "asset.png")
	if err := os.WriteFile(path, []byte("original bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	revealer := &fakeRevealer{}
	handler, err := New("127.0.0.1:7342", Dependencies{
		Status: Status{CodexState: codex.StateReady, CodexPlan: "plus"}, Catalog: catalogService,
		Managed: fakeManaged{path: path}, Revealer: revealer, CSRFSecret: bytes.Repeat([]byte{1}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}

	return handler, testDependencies{revealer: revealer}
}

func requestCatalogPage(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	return request(t, handler, http.MethodGet, path, "127.0.0.1:7342", nil)
}

type fakeCatalog struct{}

func (fakeCatalog) Search(context.Context, catalog.AssetQuery) (catalog.Page[catalog.AssetSummary], error) {
	id, _ := catalog.ParseAssetID(testAssetID)
	return catalog.Page[catalog.AssetSummary]{Items: []catalog.AssetSummary{{ID: id, Title: "Blue Knight", PrimaryType: catalog.PrimaryTypeCharacter, DisplayWidth: 32, DisplayHeight: 32, Format: "png"}}, TotalItems: 1}, nil
}

func (fakeCatalog) Get(_ context.Context, id catalog.AssetID) (catalog.AssetDetail, error) {
	if id.String() != testAssetID {
		return catalog.AssetDetail{}, catalog.ErrNotFound
	}
	return catalog.AssetDetail{AssetSummary: catalog.AssetSummary{ID: id, Title: "Blue Knight", PrimaryType: catalog.PrimaryTypeCharacter, Style: "Pixel art", Version: 1, Format: "png"}, Description: "A blue knight.", MIMEType: "image/png", OriginalFilename: "asset.png", ManagedPath: "asset.png", FileSizeBytes: int64(len("original bytes")), Layout: catalog.Layout{Kind: catalog.LayoutKindSingle}}, nil
}

func (fakeCatalog) GetThumbnail(context.Context, catalog.AssetID) (catalog.Thumbnail, error) {
	return catalog.Thumbnail{MIMEType: "image/png", Data: []byte("thumbnail"), Version: 1}, nil
}
func (fakeCatalog) GetOriginal(_ context.Context, id catalog.AssetID) (catalog.Original, error) {
	return catalog.Original{ID: id, ManagedPath: "asset.png", OriginalFilename: "asset.png", MIMEType: "image/png", FileSizeBytes: int64(len("original bytes"))}, nil
}
func (fakeCatalog) UpdateSemanticMetadata(_ context.Context, id catalog.AssetID, edit catalog.MetadataEdit) (catalog.AssetDetail, error) {
	if edit.Title == "" {
		return catalog.AssetDetail{}, &catalog.ValidationError{Fields: map[string]string{"title": "required"}}
	}
	return fakeCatalog{}.Get(context.Background(), id)
}

type queryRecordingCatalog struct {
	fakeCatalog
	query catalog.AssetQuery
}

func (catalogService *queryRecordingCatalog) Search(
	_ context.Context,
	query catalog.AssetQuery,
) (catalog.Page[catalog.AssetSummary], error) {
	catalogService.query = query

	return catalogService.fakeCatalog.Search(context.Background(), query)
}

type fakeManaged struct{ path string }

func (managed fakeManaged) OpenManaged(value importer.ManagedPath) (*os.File, error) {
	if value.String() != "asset.png" {
		return nil, errors.New("unexpected managed path")
	}
	return os.Open(managed.path)
}

type fakeRevealer struct{ path string }

func (reveal *fakeRevealer) Reveal(_ context.Context, path string) error {
	reveal.path = path
	return nil
}

var _ ProcessingReader = fakeProcessing{}

type fakeProcessing struct{}

func (fakeProcessing) Snapshot() importer.Progress {
	return importer.Progress{Active: true, StartedAt: time.Now(), ItemsTotal: 1}
}
