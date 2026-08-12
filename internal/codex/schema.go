package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dfcut8/assets-library-manager/internal/catalog"
	"github.com/dfcut8/assets-library-manager/internal/importer"
)

const analysisPrompt = `Inspect only the supplied game-development image and return only the requested structured metadata. Do not inspect unrelated host files. Describe visible content, not filenames or inferred names, ownership, licensing, or provenance. Provide up to 64 useful positive tags without filler. Use lowercase kebab-case tag slugs, omit absence-only tags, and use only the six requested facets. Treat layout independently from content type. Use null for unknown sheet measurements.`

var facetOrder = []string{"subject", "theme", "material", "viewpoint", "composition", "palette"}

type rawLayout struct {
	Kind           catalog.LayoutKind `json:"kind"`
	Columns        *int               `json:"columns"`
	Rows           *int               `json:"rows"`
	CellWidth      *int               `json:"cell_width"`
	CellHeight     *int               `json:"cell_height"`
	FrameCount     *int               `json:"frame_count"`
	AnimationLabel *string            `json:"animation_label"`
}

type rawFacets struct {
	Subject     []string `json:"subject"`
	Theme       []string `json:"theme"`
	Material    []string `json:"material"`
	Viewpoint   []string `json:"viewpoint"`
	Composition []string `json:"composition"`
	Palette     []string `json:"palette"`
}

func (facets rawFacets) values(name string) []string {
	switch name {
	case "subject":
		return facets.Subject
	case "theme":
		return facets.Theme
	case "material":
		return facets.Material
	case "viewpoint":
		return facets.Viewpoint
	case "composition":
		return facets.Composition
	case "palette":
		return facets.Palette
	default:
		return nil
	}
}

type rawAnalysis struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	PrimaryType catalog.PrimaryType `json:"primary_type"`
	Layout      rawLayout           `json:"layout"`
	Style       string              `json:"style"`
	PixelArt    bool                `json:"pixel_art"`
	Confidence  float64             `json:"confidence"`
	Facets      rawFacets           `json:"facets"`
}

type normalizedFacets struct {
	Subject     []string `json:"subject"`
	Theme       []string `json:"theme"`
	Material    []string `json:"material"`
	Viewpoint   []string `json:"viewpoint"`
	Composition []string `json:"composition"`
	Palette     []string `json:"palette"`
}

func (facets *normalizedFacets) set(name string, values []string) {
	switch name {
	case "subject":
		facets.Subject = values
	case "theme":
		facets.Theme = values
	case "material":
		facets.Material = values
	case "viewpoint":
		facets.Viewpoint = values
	case "composition":
		facets.Composition = values
	case "palette":
		facets.Palette = values
	}
}

type normalizedAnalysis struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	PrimaryType catalog.PrimaryType `json:"primary_type"`
	Layout      catalog.Layout      `json:"layout"`
	Style       string              `json:"style"`
	PixelArt    bool                `json:"pixel_art"`
	Confidence  float64             `json:"confidence"`
	Facets      normalizedFacets    `json:"facets"`
}

func decodeAndNormalize(data []byte, width, height int) (AnalysisResult, normalizedAnalysis, error) {
	if err := requireResponseShape(data); err != nil {
		return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex returned incomplete metadata", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw rawAnalysis
	if err := decoder.Decode(&raw); err != nil {
		return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex returned malformed metadata", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex returned extra metadata", err)
	}

	title := strings.TrimSpace(raw.Title)
	description := strings.TrimSpace(raw.Description)
	style := strings.TrimSpace(raw.Style)
	if !validRuneLength(title, 1, 160) || !validRuneLength(description, 1, 2000) ||
		!validRuneLength(style, 1, 80) {
		return AnalysisResult{}, normalizedAnalysis{}, invalidResponse(
			"Codex metadata text is outside allowed limits", nil,
		)
	}
	if !raw.PrimaryType.Valid() {
		return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex returned an unknown primary type", nil)
	}
	if raw.Confidence < 0 || raw.Confidence > 1 {
		return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex confidence is outside 0 to 1", nil)
	}
	for _, value := range []*int{
		raw.Layout.Columns, raw.Layout.Rows, raw.Layout.CellWidth,
		raw.Layout.CellHeight, raw.Layout.FrameCount,
	} {
		if value != nil && *value < 1 {
			return AnalysisResult{}, normalizedAnalysis{}, invalidResponse(
				"Codex returned a non-positive layout measurement", nil,
			)
		}
	}
	layout := catalog.Layout{
		Kind: raw.Layout.Kind, Columns: intValue(raw.Layout.Columns), Rows: intValue(raw.Layout.Rows),
		CellWidth: intValue(raw.Layout.CellWidth), CellHeight: intValue(raw.Layout.CellHeight),
		FrameCount: intValue(raw.Layout.FrameCount),
	}
	if raw.Layout.AnimationLabel != nil {
		layout.AnimationLabel = normalizeSlug(*raw.Layout.AnimationLabel)
		if layout.AnimationLabel == "" {
			return AnalysisResult{}, normalizedAnalysis{}, invalidResponse(
				"Codex returned an invalid animation label", nil,
			)
		}
	}
	if err := layout.Validate(width, height); err != nil {
		return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex returned an invalid layout", err)
	}

	tags := make([]importer.Tag, 0, 16)
	seen := make(map[string]struct{})
	var facets normalizedFacets
	for _, facet := range facetOrder {
		values := raw.Facets.values(facet)
		normalized := make([]string, 0, len(values))
		for _, value := range values {
			slug := normalizeSlug(value)
			if err := catalog.ValidateTag(slug); err != nil {
				return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex returned an invalid tag", err)
			}
			key := facet + "\x00" + slug
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			tag := importer.Tag{Facet: facet, Slug: slug, Label: humanizeSlug(slug), Origin: "ai"}
			if err := importer.ValidateTag(tag); err != nil {
				return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex returned an invalid tag", err)
			}
			tags = append(tags, tag)
			normalized = append(normalized, slug)
			if len(tags) > 64 {
				return AnalysisResult{}, normalizedAnalysis{}, invalidResponse("Codex returned too many tags", nil)
			}
		}
		facets.set(facet, normalized)
	}
	normalized := normalizedAnalysis{
		Title: title, Description: description, PrimaryType: raw.PrimaryType,
		Layout: layout, Style: style, PixelArt: raw.PixelArt, Confidence: raw.Confidence,
		Facets: facets,
	}
	result := AnalysisResult{
		Title: title, Description: description, PrimaryType: raw.PrimaryType,
		Layout: layout, Style: style, PixelArt: raw.PixelArt, Confidence: raw.Confidence,
		Tags: tags,
	}

	return result, normalized, nil
}

func requireResponseShape(data []byte) error {
	uniqueDecoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanUniqueJSONValue(uniqueDecoder); err != nil {
		return err
	}
	if err := requireJSONEOF(uniqueDecoder); err != nil {
		return err
	}
	top, err := requiredObject(data, []string{
		"title", "description", "primary_type", "layout", "style", "pixel_art", "confidence", "facets",
	}, false)
	if err != nil {
		return err
	}
	if _, err := requiredObject(top["layout"], []string{
		"kind", "columns", "rows", "cell_width", "cell_height", "frame_count", "animation_label",
	}, true); err != nil {
		return err
	}
	if _, err := requiredObject(top["facets"], facetOrder, false); err != nil {
		return err
	}

	return nil
}

func scanUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("json object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return errors.New("json object key is duplicated")
			}
			keys[key] = struct{}{}
			if err := scanUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()

		return err
	case '[':
		for decoder.More() {
			if err := scanUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()

		return err
	default:
		return errors.New("unexpected json delimiter")
	}
}

func requiredObject(data []byte, fields []string, allowNullFields bool) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("required value is not an object")
	}
	for _, field := range fields {
		value, exists := object[field]
		if !exists {
			return nil, errors.New("required field is missing")
		}
		if !allowNullFields && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errors.New("required field is null")
		}
	}

	return object, nil
}

func outputSchema() map[string]any {
	nullableInteger := func() map[string]any {
		return map[string]any{"type": []string{"integer", "null"}, "minimum": 1}
	}
	nullableSlug := map[string]any{
		"type": []string{"string", "null"}, "maxLength": 64,
		"pattern": `^[a-z0-9]+(?:-[a-z0-9]+)*$`,
	}
	tagArray := func() map[string]any {
		return map[string]any{
			"type": "array", "maxItems": 64, "uniqueItems": true,
			"items": map[string]any{
				"type": "string", "minLength": 1, "maxLength": 64,
				"pattern": `^[a-z0-9]+(?:-[a-z0-9]+)*$`,
			},
		}
	}

	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{
			"title", "description", "primary_type", "layout", "style", "pixel_art", "confidence", "facets",
		},
		"properties": map[string]any{
			"title":       map[string]any{"type": "string", "minLength": 1, "maxLength": 160},
			"description": map[string]any{"type": "string", "minLength": 1, "maxLength": 2000},
			"primary_type": map[string]any{
				"type": "string", "enum": []string{
					"character", "creature", "terrain", "environment", "prop", "building", "vehicle",
					"weapon-tool", "collectible", "ui", "icon", "background", "texture-material", "vfx", "decal", "other",
				},
			},
			"layout": map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []string{
					"kind", "columns", "rows", "cell_width", "cell_height", "frame_count", "animation_label",
				},
				"properties": map[string]any{
					"kind": map[string]any{
						"type": "string", "enum": []string{"single", "sprite-sheet", "tile-sheet"},
					},
					"columns": nullableInteger(), "rows": nullableInteger(),
					"cell_width": nullableInteger(), "cell_height": nullableInteger(),
					"frame_count": nullableInteger(), "animation_label": nullableSlug,
				},
			},
			"style":      map[string]any{"type": "string", "minLength": 1, "maxLength": 80},
			"pixel_art":  map[string]any{"type": "boolean"},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"facets": map[string]any{
				"type": "object", "additionalProperties": false,
				"required": facetOrder,
				"properties": map[string]any{
					"subject": tagArray(), "theme": tagArray(), "material": tagArray(),
					"viewpoint": tagArray(), "composition": tagArray(), "palette": tagArray(),
				},
			},
		},
	}
}

func invalidResponse(message string, cause error) *AnalysisError {
	return newAnalysisError(ErrorInvalidResponse, message, true, cause)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}

	return errors.New("more than one JSON value")
}

func validRuneLength(value string, minimum, maximum int) bool {
	if !utf8.ValidString(value) {
		return false
	}
	length := utf8.RuneCountInString(value)

	return length >= minimum && length <= maximum
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}

	return *value
}

func normalizeSlug(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var builder strings.Builder
	separator := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			if separator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			builder.WriteRune(char)
			separator = false
		case char == '-' || char == '_' || unicode.IsSpace(char):
			separator = true
		default:
			return ""
		}
	}

	return builder.String()
}

func humanizeSlug(slug string) string {
	words := strings.Split(slug, "-")
	for index, word := range words {
		if word == "" {
			continue
		}
		words[index] = strings.ToUpper(word[:1]) + word[1:]
	}

	return strings.Join(words, " ")
}

func safeUsageJSON(raw json.RawMessage) string {
	if len(raw) == 0 || len(raw) > 4096 || !json.Valid(raw) {
		return ""
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return ""
	}
	if !numericUsageObject(object, 0) {
		return ""
	}
	data, err := json.Marshal(object)
	if err != nil || len(data) > 4096 {
		return ""
	}

	return string(data)
}

func numericUsageObject(object map[string]any, depth int) bool {
	if depth > 4 || len(object) > 64 {
		return false
	}
	for key, value := range object {
		if key == "" || len(key) > 64 {
			return false
		}
		switch typed := value.(type) {
		case float64, nil:
		case map[string]any:
			if !numericUsageObject(typed, depth+1) {
				return false
			}
		default:
			return false
		}
	}

	return true
}
