package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/importer"
)

type server struct {
	allowedHost  string
	dependencies Dependencies
	templates    *template.Template
	csrf         csrfProtection
}

type pageData struct {
	Status     Status
	CSRFToken  string
	Query      catalog.AssetQuery
	Results    catalog.Page[catalog.AssetSummary]
	Detail     *catalog.AssetDetail
	Processing importer.Progress
	Form       metadataForm
	Message    string
	Errors     map[string]string
}

type metadataForm struct {
	Version     int
	Title       string
	Description string
	PrimaryType catalog.PrimaryType
	Style       string
	PixelArt    bool
	Layout      catalog.Layout
	Tags        []catalog.Tag
}

func (s *server) catalog(w http.ResponseWriter, r *http.Request) {
	query, err := parseAssetQuery(r)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid catalog filters.")
		return
	}
	results, err := s.search(r, query)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Unable to load the catalog.")
		return
	}
	data := pageData{
		Status: s.dependencies.Status, CSRFToken: csrfToken(r.Context()), Query: query,
		Results: results, Processing: s.snapshot(),
	}
	if isHTMX(r) {
		s.render(w, "catalog-results", data)
		return
	}
	s.render(w, "assets.html", data)
}

func (s *server) catalogFragment(w http.ResponseWriter, r *http.Request) {
	query, err := parseAssetQuery(r)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid catalog filters.")
		return
	}
	results, err := s.search(r, query)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Unable to load the catalog.")
		return
	}
	s.render(w, "catalog-results", pageData{Query: query, Results: results})
}

func (s *server) detail(w http.ResponseWriter, r *http.Request) {
	detail, ok := s.loadDetail(w, r)
	if !ok {
		return
	}
	data := pageData{
		Status: s.dependencies.Status, CSRFToken: csrfToken(r.Context()), Detail: &detail,
		Processing: s.snapshot(), Form: formFromDetail(detail),
	}
	if isHTMX(r) {
		s.render(w, "asset-detail", data)
		return
	}
	s.render(w, "detail.html", data)
}

func (s *server) thumbnail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	ctx, cancel := handlerContext(r)
	defer cancel()
	thumbnail, err := s.dependencies.Catalog.GetThumbnail(ctx, id)
	if errors.Is(err, catalog.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Unable to load thumbnail.", http.StatusInternalServerError)
		return
	}
	if thumbnail.MIMEType != "image/png" || len(thumbnail.Data) == 0 {
		http.Error(w, "Thumbnail is unavailable.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", thumbnail.MIMEType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("ETag", fmt.Sprintf("\"thumbnail-%s-%d\"", id, thumbnail.Version))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(thumbnail.Data)
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	ctx, cancel := handlerContext(r)
	defer cancel()
	original, err := s.dependencies.Catalog.GetOriginal(ctx, id)
	if errors.Is(err, catalog.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Unable to load original.", http.StatusInternalServerError)
		return
	}
	managedPath, err := importer.NewManagedPath(original.ManagedPath)
	if err != nil || !validOriginalMIME(original.MIMEType) {
		http.Error(w, "Original is unavailable.", http.StatusInternalServerError)
		return
	}
	file, err := s.dependencies.Managed.OpenManaged(managedPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			return
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != original.FileSizeBytes {
		http.Error(w, "Original is unavailable.", http.StatusNotFound)
		return
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": safeFilename(original.OriginalFilename)})
	w.Header().Set("Content-Type", original.MIMEType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func (s *server) metadata(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, metadataBodyLimit)
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusRequestEntityTooLarge, "Metadata form is too large.")
		return
	}
	form, edit, formErrs := parseMetadataForm(r)
	if len(formErrs) != 0 {
		s.renderMetadataResult(w, r, form, formErrs, "", http.StatusUnprocessableEntity)
		return
	}
	ctx, cancel := handlerContext(r)
	defer cancel()
	detail, err := s.dependencies.Catalog.UpdateSemanticMetadata(ctx, id, edit)
	if err == nil {
		form = formFromDetail(detail)
		s.renderMetadataResult(w, r, form, nil, "Metadata saved.", http.StatusOK)
		return
	}
	var validation *catalog.ValidationError
	if errors.As(err, &validation) {
		s.renderMetadataResult(w, r, form, validation.Fields, "", http.StatusUnprocessableEntity)
		return
	}
	var stale *catalog.StaleEditError
	if errors.As(err, &stale) {
		s.renderMetadataResult(w, r, form, map[string]string{
			"version": "This asset changed in another edit. Reload and apply your changes again.",
		}, "", http.StatusConflict)
		return
	}
	if errors.Is(err, catalog.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	s.renderError(w, r, http.StatusInternalServerError, "Unable to save metadata.")
}

func (s *server) reveal(w http.ResponseWriter, r *http.Request) {
	s.openOriginal(w, r, s.dependencies.Files.Reveal, "asset-revealed")
}

func (s *server) view(w http.ResponseWriter, r *http.Request) {
	s.openOriginal(w, r, s.dependencies.Files.Open, "asset-opened")
}

func (s *server) openOriginal(
	w http.ResponseWriter,
	r *http.Request,
	launch func(context.Context, string) error,
	event string,
) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, fileActionBodyLimit)
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusRequestEntityTooLarge, "File action request is too large.")
		return
	}
	ctx, cancel := handlerContext(r)
	defer cancel()
	original, err := s.dependencies.Catalog.GetOriginal(ctx, id)
	if errors.Is(err, catalog.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Unable to open the original.")
		return
	}
	managedPath, err := importer.NewManagedPath(original.ManagedPath)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Unable to open the original.")
		return
	}
	file, err := s.dependencies.Managed.OpenManaged(managedPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	filePath := file.Name()
	if closeErr := file.Close(); closeErr != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Unable to open the original.")
		return
	}
	if err := launch(ctx, filePath); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Unable to open the original.")
		return
	}
	if isHTMX(r) {
		w.Header().Set("HX-Trigger", event)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/assets/"+id.String(), http.StatusSeeOther)
}

func (s *server) processing(w http.ResponseWriter, r *http.Request) {
	data := pageData{Processing: s.snapshot()}
	if isHTMX(r) {
		s.render(w, "processing-status", data)
		return
	}
	s.render(w, "processing.html", data)
}

func (s *server) search(r *http.Request, query catalog.AssetQuery) (catalog.Page[catalog.AssetSummary], error) {
	ctx, cancel := handlerContext(r)
	defer cancel()

	return s.dependencies.Catalog.Search(ctx, query)
}

func (s *server) loadDetail(w http.ResponseWriter, r *http.Request) (catalog.AssetDetail, bool) {
	id, ok := s.parseID(w, r)
	if !ok {
		return catalog.AssetDetail{}, false
	}
	ctx, cancel := handlerContext(r)
	defer cancel()
	detail, err := s.dependencies.Catalog.Get(ctx, id)
	if errors.Is(err, catalog.ErrNotFound) {
		http.NotFound(w, r)
		return catalog.AssetDetail{}, false
	}
	if err != nil {
		http.Error(w, "Unable to load asset.", http.StatusInternalServerError)
		return catalog.AssetDetail{}, false
	}

	return detail, true
}

func (s *server) parseID(w http.ResponseWriter, r *http.Request) (catalog.AssetID, bool) {
	id, err := catalog.ParseAssetID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return "", false
	}

	return id, true
}

func (s *server) snapshot() importer.Progress {
	if s.dependencies.Processing == nil {
		return importer.Progress{}
	}

	return s.dependencies.Processing.Snapshot()
}

func (s *server) renderMetadataResult(
	w http.ResponseWriter,
	r *http.Request,
	form metadataForm,
	fieldErrors map[string]string,
	message string,
	status int,
) {
	detail, ok := s.loadDetail(w, r)
	if !ok {
		return
	}
	data := pageData{CSRFToken: csrfToken(r.Context()), Detail: &detail, Form: form, Errors: fieldErrors, Message: message}
	if isHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		s.render(w, "metadata-form", data)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, "detail.html", data)
}

func (s *server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if isHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		s.render(w, "request-error", pageData{Message: message})
		return
	}
	http.Error(w, message, status)
}

func (s *server) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		return
	}
}

func handlerContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), requestTimeout)
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func validOriginalMIME(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func safeFilename(value string) string {
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f || character == '/' || character == '\\' {
			return -1
		}
		return character
	}, value)
	value = strings.TrimSpace(value)
	if value == "" {
		return "asset"
	}

	return value
}

func formFromDetail(detail catalog.AssetDetail) metadataForm {
	return metadataForm{
		Version: detail.Version, Title: detail.Title, Description: detail.Description,
		PrimaryType: detail.PrimaryType, Style: detail.Style, PixelArt: detail.PixelArt,
		Layout: detail.Layout, Tags: append([]catalog.Tag(nil), detail.Tags...),
	}
}

func parseMetadataForm(r *http.Request) (metadataForm, catalog.MetadataEdit, map[string]string) {
	form := metadataForm{
		Title: r.PostForm.Get("title"), Description: r.PostForm.Get("description"),
		PrimaryType: catalog.PrimaryType(r.PostForm.Get("primary_type")), Style: r.PostForm.Get("style"),
		PixelArt: r.PostForm.Get("pixel_art") == "true", Layout: catalog.Layout{
			Kind: catalog.LayoutKind(r.PostForm.Get("layout_kind")), AnimationLabel: r.PostForm.Get("animation_label"),
		},
	}
	fields := make(map[string]string)
	form.Version = parsePositiveInt(r.PostForm.Get("version"), "version", fields)
	form.Layout.Columns = parseNonNegativeInt(r.PostForm.Get("columns"), "columns", fields)
	form.Layout.Rows = parseNonNegativeInt(r.PostForm.Get("rows"), "rows", fields)
	form.Layout.CellWidth = parseNonNegativeInt(r.PostForm.Get("cell_width"), "cell_width", fields)
	form.Layout.CellHeight = parseNonNegativeInt(r.PostForm.Get("cell_height"), "cell_height", fields)
	form.Layout.FrameCount = parseNonNegativeInt(r.PostForm.Get("frame_count"), "frame_count", fields)
	for index, facet := range r.PostForm["tag_facet"] {
		slugs := r.PostForm["tag_slug"]
		labels := r.PostForm["tag_label"]
		if index >= len(slugs) || index >= len(labels) {
			fields["tags"] = "tag fields are incomplete"
			break
		}
		form.Tags = append(form.Tags, catalog.Tag{Facet: facet, Slug: slugs[index], Label: labels[index]})
	}
	if len(r.PostForm["tag_slug"]) != len(form.Tags) || len(r.PostForm["tag_label"]) != len(form.Tags) {
		fields["tags"] = "tag fields are incomplete"
	}
	edit := catalog.MetadataEdit{
		Version: form.Version, Title: form.Title, Description: form.Description, PrimaryType: form.PrimaryType,
		Style: form.Style, PixelArt: form.PixelArt, Layout: form.Layout, Tags: form.Tags,
	}
	if len(fields) != 0 {
		return form, edit, fields
	}

	return form, edit, nil
}

func parsePositiveInt(value, field string, fields map[string]string) int {
	parsed := parseNonNegativeInt(value, field, fields)
	if parsed < 1 && fields[field] == "" {
		fields[field] = field + " must be positive"
	}

	return parsed
}

func parseNonNegativeInt(value, field string, fields map[string]string) int {
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		fields[field] = field + " must be a non-negative integer"
		return 0
	}

	return parsed
}

func parseAssetQuery(r *http.Request) (catalog.AssetQuery, error) {
	values := r.URL.Query()
	query := catalog.AssetQuery{
		Q: values.Get("q"), Sort: catalog.Sort(values.Get("sort")),
		Styles: queryList(values["style"], false), Orientations: values["orientation"], Formats: values["format"],
	}
	var err error
	if query.Page, err = queryInt(values.Get("page")); err != nil {
		return catalog.AssetQuery{}, err
	}
	if query.PageSize, err = queryInt(values.Get("page_size")); err != nil {
		return catalog.AssetQuery{}, err
	}
	for _, value := range queryList(values["type"], true) {
		query.Types = append(query.Types, catalog.PrimaryType(value))
	}
	for _, value := range values["tag"] {
		tag, err := catalog.ParseTagFilter(value)
		if err != nil {
			return catalog.AssetQuery{}, err
		}
		query.Tags = append(query.Tags, tag)
	}
	for key, destination := range map[string]**bool{
		"pixel_art": &query.PixelArt, "transparency": &query.Transparency, "animated": &query.Animated,
	} {
		value, present, err := queryBool(values, key)
		if err != nil {
			return catalog.AssetQuery{}, err
		}
		if present {
			*destination = &value
		}
	}
	for key, destination := range map[string]*int{
		"min_width": &query.MinWidth, "max_width": &query.MaxWidth,
		"min_height": &query.MinHeight, "max_height": &query.MaxHeight,
	} {
		parsed, err := queryInt(values.Get(key))
		if err != nil {
			return catalog.AssetQuery{}, err
		}
		*destination = parsed
	}
	if query.ImportedFrom, err = queryDate(values.Get("imported_from"), false); err != nil {
		return catalog.AssetQuery{}, err
	}
	if query.ImportedTo, err = queryDate(values.Get("imported_to"), true); err != nil {
		return catalog.AssetQuery{}, err
	}

	return catalog.NormalizeAssetQuery(query)
}

func queryList(values []string, lowercase bool) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		for item := range strings.SplitSeq(value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if lowercase {
				item = strings.ToLower(item)
			}
			result = append(result, item)
		}
	}

	return result
}

func queryInt(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, errors.New("query number is invalid")
	}

	return parsed, nil
}

func queryBool(values url.Values, key string) (bool, bool, error) {
	value, present := values[key]
	if !present {
		return false, false, nil
	}
	if len(value) != 1 {
		return false, false, errors.New("query boolean is repeated")
	}
	parsed, err := strconv.ParseBool(value[0])
	if err != nil {
		return false, false, errors.New("query boolean is invalid")
	}

	return parsed, true, nil
}

func queryDate(value string, end bool) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, errors.New("query date is invalid")
	}
	if end {
		parsed = parsed.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}

	return &parsed, nil
}
