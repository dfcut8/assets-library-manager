package codex

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeAndNormalize(t *testing.T) {
	input := strings.Replace(validAnalysisJSON, `"forest-knight"`, `"forest knight"`, 1)
	result, normalized, err := decodeAndNormalize([]byte(input), 64, 64)
	if err != nil {
		t.Fatalf("decodeAndNormalize() error = %v", err)
	}
	if len(result.Tags) != 4 || normalized.Facets.Subject[0] != "forest-knight" {
		t.Fatalf("decodeAndNormalize() = %#v, %#v", result, normalized)
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "forest knight") {
		t.Fatalf("normalized JSON contains unnormalized tag: %s", data)
	}
}

func TestDecodeAndNormalizeRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "unknown field", value: strings.Replace(validAnalysisJSON, `"title":`, `"extra":true,"title":`, 1)},
		{name: "unknown type", value: strings.Replace(validAnalysisJSON, `"character"`, `"document"`, 1)},
		{name: "bad confidence", value: strings.Replace(validAnalysisJSON, `0.91`, `1.1`, 1)},
		{name: "missing confidence", value: strings.Replace(validAnalysisJSON, `"confidence":0.91,`, ``, 1)},
		{name: "duplicate confidence", value: strings.Replace(validAnalysisJSON, `"confidence":0.91,`, `"confidence":0.8,"confidence":0.91,`, 1)},
		{name: "null facet", value: strings.Replace(validAnalysisJSON, `"material":[]`, `"material":null`, 1)},
		{name: "bad tag", value: strings.Replace(validAnalysisJSON, `"fantasy"`, `"fantasy!"`, 1)},
		{name: "zero layout measurement", value: strings.Replace(validAnalysisJSON, `"columns":null`, `"columns":0`, 1)},
		{name: "sheet exceeds image", value: strings.Replace(
			validAnalysisJSON,
			`"kind":"single","columns":null,"rows":null,"cell_width":null,"cell_height":null,"frame_count":null`,
			`"kind":"sprite-sheet","columns":100,"rows":2,"cell_width":64,"cell_height":32,"frame_count":2`,
			1,
		)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := decodeAndNormalize([]byte(tt.value), 64, 64)
			var classified *AnalysisError
			if err == nil || !errors.As(err, &classified) || classified.Kind != ErrorInvalidResponse {
				t.Fatalf("decodeAndNormalize() error = %#v", err)
			}
		})
	}
}

func TestSanitizeDiagnosticIsBoundedAndSingleLine(t *testing.T) {
	value := sanitizeDiagnostic(" secret\n\t" + strings.Repeat("x", maxDiagnosticBytes+100))
	if len(value) > maxDiagnosticBytes || strings.ContainsAny(value, "\n\t") || strings.Contains(value, "  ") {
		t.Fatalf("sanitizeDiagnostic() returned unsafe value of length %d", len(value))
	}
}

func TestSafeUsageJSONAllowsOnlyBoundedNumericUsage(t *testing.T) {
	if got := safeUsageJSON(json.RawMessage(`{"total":{"inputTokens":10},"modelContextWindow":1000}`)); got == "" {
		t.Fatal("safeUsageJSON() rejected numeric usage")
	}
	if got := safeUsageJSON(json.RawMessage(`{"providerResponse":"secret"}`)); got != "" {
		t.Fatalf("safeUsageJSON() retained provider text: %q", got)
	}
}

func TestOutputSchemaDoesNotUseUnsupportedUniqueItems(t *testing.T) {
	data, err := json.Marshal(outputSchema())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"uniqueItems"`) {
		t.Fatalf("output schema contains unsupported uniqueItems: %s", data)
	}
}

func FuzzDecodeAndNormalize(f *testing.F) {
	f.Add([]byte(validAnalysisJSON), 64, 64)
	f.Add([]byte(`{"title":null}`), 1, 1)
	f.Fuzz(func(_ *testing.T, data []byte, width, height int) {
		if width < 1 || width > 4096 || height < 1 || height > 4096 || len(data) > maxMessageBytes {
			return
		}
		_, _, _ = decodeAndNormalize(data, width, height)
	})
}
