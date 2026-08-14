package importer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

func TestRecordItemResultLogsPerItemAndCycleTokenUsage(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	startedAt := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	coordinator := &Coordinator{
		logger: slog.New(slog.NewJSONHandler(&output, nil)),
		now:    func() time.Time { return startedAt.Add(time.Second) },
		progress: Progress{
			Active: true, StartedAt: startedAt, ItemsTotal: 2,
		},
	}
	coordinator.recordItemResult(t.Context(), itemResult{
		path: newTestSourcePath(t, "first.png"), state: ItemStateReady,
		tokenUsage: TokenUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
	})
	coordinator.recordItemResult(t.Context(), itemResult{
		path: newTestSourcePath(t, "second.png"), state: ItemStateFailed,
		tokenUsage: TokenUsage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10},
	})
	coordinator.completeProgress(t.Context(), "completed")

	type record struct {
		Message      string `json:"msg"`
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
		TotalTokens  int64  `json:"total_tokens"`
	}
	itemTotals := []int64{}
	var cycle record
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var entry record
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("decoding log record: %v", err)
		}
		switch entry.Message {
		case "import item processing completed":
			itemTotals = append(itemTotals, entry.TotalTokens)
		case "startup import processing cycle completed":
			cycle = entry
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(itemTotals) != 2 || itemTotals[0] != 30 || itemTotals[1] != 10 {
		t.Fatalf("per-item token totals = %v, want [30 10]", itemTotals)
	}
	if cycle.InputTokens != 14 || cycle.OutputTokens != 26 || cycle.TotalTokens != 40 {
		t.Fatalf("cycle token usage = %#v, want input=14 output=26 total=40", cycle)
	}
}

func newTestSourcePath(t *testing.T, value string) SourcePath {
	t.Helper()
	result, err := NewSourcePath(value)
	if err != nil {
		t.Fatal(err)
	}

	return result
}
