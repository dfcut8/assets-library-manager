package catalog

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maximumQueryRunes  = 500
	maximumQueryTokens = 64
	maximumFilterItems = 64
)

// ParseTagFilter parses one required facet:slug query value.
func ParseTagFilter(value string) (TagFilter, error) {
	facet, slug, found := strings.Cut(value, ":")
	if !found {
		return TagFilter{}, validationError("tag", "tag filter must use facet:slug")
	}
	filters, err := normalizeTagFilters([]TagFilter{{Facet: facet, Slug: slug}})
	if err != nil {
		return TagFilter{}, err
	}

	return filters[0], nil
}

// NormalizeAssetQuery applies defaults, deduplicates filters, and validates bounds.
func NormalizeAssetQuery(query AssetQuery) (AssetQuery, error) {
	query.Q = strings.TrimSpace(query.Q)
	if !utf8.ValidString(query.Q) || utf8.RuneCountInString(query.Q) > maximumQueryRunes {
		return AssetQuery{}, validationError("q", "search text must be valid UTF-8 and at most 500 characters")
	}
	if query.Page < 0 || query.PageSize < 0 {
		return AssetQuery{}, validationError("page", "page values must not be negative")
	}
	if query.Page == 0 {
		query.Page = 1
	}
	if query.PageSize == 0 {
		query.PageSize = DefaultPageSize
	}
	query.PageSize = min(query.PageSize, MaximumPageSize)
	maximumInt := int(^uint(0) >> 1)
	if query.Page-1 > maximumInt/query.PageSize {
		return AssetQuery{}, validationError("page", "page offset is too large")
	}
	if query.Sort == "" {
		query.Sort = SortNewest
	}
	if !query.Sort.Valid() {
		return AssetQuery{}, validationError("sort", "sort is unsupported")
	}
	if query.Q != "" {
		fts, err := FTSLiteralQuery(query.Q)
		if err != nil {
			return AssetQuery{}, err
		}
		if fts == "" {
			query.Q = ""
		}
	}
	if query.Sort == SortRelevance && query.Q == "" {
		query.Sort = SortNewest
	}

	var err error
	query.Types, err = normalizePrimaryTypes(query.Types)
	if err != nil {
		return AssetQuery{}, err
	}
	query.Styles, err = normalizeStrings("style", query.Styles, 80)
	if err != nil {
		return AssetQuery{}, err
	}
	query.Orientations, err = normalizeControlled(
		"orientation", query.Orientations, []string{"square", "portrait", "landscape"},
	)
	if err != nil {
		return AssetQuery{}, err
	}
	query.Formats, err = normalizeControlled(
		"format", query.Formats, []string{"png", "jpeg", "webp", "gif"},
	)
	if err != nil {
		return AssetQuery{}, err
	}
	query.Tags, err = normalizeTagFilters(query.Tags)
	if err != nil {
		return AssetQuery{}, err
	}
	if err := validateNumericBounds(query); err != nil {
		return AssetQuery{}, err
	}
	if err := validateDateBounds(query); err != nil {
		return AssetQuery{}, err
	}

	return query, nil
}

// FTSLiteralQuery converts free text to quoted literal FTS5 tokens joined only by AND.
func FTSLiteralQuery(value string) (string, error) {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximumQueryRunes {
		return "", validationError("q", "search text must be valid UTF-8 and at most 500 characters")
	}
	tokens := strings.FieldsFunc(value, func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsNumber(character)
	})
	if len(tokens) > maximumQueryTokens {
		return "", validationError("q", "search text contains too many terms")
	}
	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "" {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(token, `"`, `""`)+`"`)
	}

	return strings.Join(quoted, " AND "), nil
}

func normalizePrimaryTypes(values []PrimaryType) ([]PrimaryType, error) {
	if len(values) > maximumFilterItems {
		return nil, validationError("type", "too many type filters")
	}
	seen := make(map[PrimaryType]struct{}, len(values))
	result := make([]PrimaryType, 0, len(values))
	for _, value := range values {
		if !value.Valid() {
			return nil, validationError("type", "primary type is unsupported")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	return result, nil
}

func normalizeStrings(field string, values []string, maximumRunes int) ([]string, error) {
	if len(values) > maximumFilterItems {
		return nil, validationError(field, "too many filter values")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximumRunes {
			return nil, validationError(field, "filter value is invalid")
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}

	return result, nil
}

func normalizeControlled(field string, values, allowed []string) ([]string, error) {
	values, err := normalizeStrings(field, values, 64)
	if err != nil {
		return nil, err
	}
	for index, value := range values {
		value = strings.ToLower(value)
		if !slices.Contains(allowed, value) {
			return nil, validationError(field, "filter value is unsupported")
		}
		values[index] = value
	}

	return values, nil
}

func normalizeTagFilters(values []TagFilter) ([]TagFilter, error) {
	if len(values) > maximumFilterItems {
		return nil, validationError("tag", "too many tag filters")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]TagFilter, 0, len(values))
	for _, value := range values {
		value.Facet = strings.TrimSpace(value.Facet)
		value.Slug = strings.TrimSpace(value.Slug)
		if err := validateFacet(value.Facet); err != nil {
			return nil, validationError("tag", err.Error())
		}
		if err := ValidateTag(value.Slug); err != nil {
			return nil, validationError("tag", "tag slug is invalid")
		}
		key := value.Facet + "\x00" + value.Slug
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}

	return result, nil
}

func validateNumericBounds(query AssetQuery) error {
	values := []struct {
		field string
		value int
	}{
		{"min_width", query.MinWidth}, {"max_width", query.MaxWidth},
		{"min_height", query.MinHeight}, {"max_height", query.MaxHeight},
	}
	for _, value := range values {
		if value.value < 0 {
			return validationError(value.field, "dimension bound must not be negative")
		}
	}
	if query.MaxWidth > 0 && query.MinWidth > query.MaxWidth {
		return validationError("max_width", "maximum width must not be less than minimum width")
	}
	if query.MaxHeight > 0 && query.MinHeight > query.MaxHeight {
		return validationError("max_height", "maximum height must not be less than minimum height")
	}

	return nil
}

func validateDateBounds(query AssetQuery) error {
	for field, value := range map[string]*time.Time{
		"imported_from": query.ImportedFrom,
		"imported_to":   query.ImportedTo,
	} {
		if value == nil {
			continue
		}
		_, offset := value.Zone()
		if offset != 0 {
			return validationError(field, "import date bound must use UTC")
		}
	}
	if query.ImportedFrom != nil && query.ImportedTo != nil && query.ImportedFrom.After(*query.ImportedTo) {
		return validationError("imported_to", "end date must not precede start date")
	}

	return nil
}

func validateFacet(value string) error {
	if value == "" || len(value) > 64 || !utf8.ValidString(value) {
		return errors.New("tag facet must contain 1 to 64 UTF-8 bytes")
	}
	for _, character := range value {
		isLowerLetter := unicode.IsLower(character)
		isDigit := unicode.IsDigit(character)
		if !isLowerLetter && !isDigit && character != '-' {
			return fmt.Errorf("tag facet %q must use lowercase kebab-case", value)
		}
	}
	if strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") || strings.Contains(value, "--") {
		return errors.New("tag facet must use lowercase kebab-case")
	}

	return nil
}
