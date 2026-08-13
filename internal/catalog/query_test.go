package catalog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAssetQueryAppliesDefaultsCapsAndDeduplicates(t *testing.T) {
	t.Parallel()
	query, err := NormalizeAssetQuery(AssetQuery{
		PageSize: 999,
		Types:    []PrimaryType{PrimaryTypeCharacter, PrimaryTypeCharacter},
		Styles:   []string{" Pixel Art ", "pixel art"},
		Formats:  []string{"PNG", "png"},
		Tags: []TagFilter{
			{Facet: "subject", Slug: "hero"},
			{Facet: "subject", Slug: "hero"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if query.Page != 1 || query.PageSize != MaximumPageSize || query.Sort != SortNewest {
		t.Fatalf("normalized pagination = %+v", query)
	}
	if len(query.Types) != 1 || len(query.Styles) != 1 ||
		len(query.Formats) != 1 || len(query.Tags) != 1 {
		t.Fatalf("normalized filters = %+v", query)
	}
}

func FuzzFTSLiteralQuery(f *testing.F) {
	for _, seed := range []string{"blue castle", `OR NOT "quoted"`, "石の城", "---"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		query, err := FTSLiteralQuery(value)
		if err != nil || query == "" {
			return
		}
		for _, part := range strings.Split(query, " AND ") {
			if len(part) < 2 || part[0] != '"' || part[len(part)-1] != '"' {
				t.Fatalf("unquoted FTS token %q in %q", part, query)
			}
		}
	})
}

func TestNormalizeAssetQueryRejectsInvalidBoundsAndNonUTCDate(t *testing.T) {
	t.Parallel()
	nonUTC := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	tests := []struct {
		name  string
		query AssetQuery
	}{
		{name: "negative page", query: AssetQuery{Page: -1}},
		{name: "overflowing offset", query: AssetQuery{Page: int(^uint(0) >> 1)}},
		{name: "reversed width", query: AssetQuery{MinWidth: 20, MaxWidth: 10}},
		{name: "non UTC date", query: AssetQuery{ImportedFrom: &nonUTC}},
		{name: "invalid tag", query: AssetQuery{Tags: []TagFilter{{Facet: "Subject", Slug: "hero"}}}},
		{name: "unsupported sort", query: AssetQuery{Sort: Sort("title; DROP TABLE assets")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NormalizeAssetQuery(test.query); !errors.Is(err, ErrInvalid) {
				t.Fatalf("NormalizeAssetQuery() error = %v", err)
			}
		})
	}
}

func TestFTSLiteralQueryQuotesTokensAndNeutralizesOperators(t *testing.T) {
	t.Parallel()
	got, err := FTSLiteralQuery(`blue OR castle -draft "quoted"`)
	if err != nil {
		t.Fatal(err)
	}
	want := `"blue" AND "OR" AND "castle" AND "draft" AND "quoted"`
	if got != want {
		t.Fatalf("FTSLiteralQuery() = %q, want %q", got, want)
	}

	query, err := NormalizeAssetQuery(AssetQuery{Q: `---`, Sort: SortRelevance})
	if err != nil {
		t.Fatal(err)
	}
	if query.Q != "" || query.Sort != SortNewest {
		t.Fatalf("punctuation-only query = %+v", query)
	}
}

func TestParseTagFilterRequiresValidatedFacetAndSlug(t *testing.T) {
	t.Parallel()
	filter, err := ParseTagFilter("material:weathered-stone")
	if err != nil {
		t.Fatal(err)
	}
	if filter.Facet != "material" || filter.Slug != "weathered-stone" {
		t.Fatalf("filter = %+v", filter)
	}
	for _, value := range []string{"material", "Material:stone", "material:not-stone", "material:stone:gray"} {
		if _, err := ParseTagFilter(value); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseTagFilter(%q) error = %v", value, err)
		}
	}
}
