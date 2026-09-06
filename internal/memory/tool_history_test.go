package memory

import (
	"strings"
	"testing"
)

func TestToolHistoryRoundTripAndSearchProjection(t *testing.T) {
	history := ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{{
		AssistantContent: "Checking now.",
		Calls: []ToolHistoryCall{{
			Name: "weather.current", Arguments: map[string]interface{}{"city": "Louisville", "private_argument": "do-not-index"},
			Status: "succeeded", Outcome: "productive", Result: "Forecast marker zephyr", ExecutedAt: "2026-08-28T12:00:00Z", SearchResult: true,
		}},
	}}}
	encoded, searchText, err := EncodeToolHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(searchText, "weather.current") || !strings.Contains(searchText, "zephyr") || strings.Contains(searchText, "do-not-index") {
		t.Fatalf("search projection = %q", searchText)
	}
	decoded, err := DecodeToolHistory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Batches) != 1 || decoded.Batches[0].Calls[0].Arguments["city"] != "Louisville" {
		t.Fatalf("decoded history = %+v", decoded)
	}
}

func TestToolHistoryBoundsAggregateResultsWithoutBreakingCorrelation(t *testing.T) {
	history := EmptyToolHistory()
	for i := 0; i < 10; i++ {
		history.Batches = append(history.Batches, ToolHistoryBatch{Calls: []ToolHistoryCall{{
			Name: "large.tool", Arguments: map[string]interface{}{"value": strings.Repeat("a", 8000)}, Status: "succeeded",
			Result: strings.Repeat("r", 16000), ExecutedAt: "2026-08-28T12:00:00Z", SearchResult: true,
		}}})
	}
	encoded, _, err := EncodeToolHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxToolHistoryBytes {
		t.Fatalf("encoded history bytes=%d", len(encoded))
	}
	decoded, err := DecodeToolHistory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Batches) != 10 {
		t.Fatalf("batch count=%d", len(decoded.Batches))
	}
	for _, batch := range decoded.Batches {
		if len(batch.Calls) != 1 || strings.TrimSpace(batch.Calls[0].Result) == "" {
			t.Fatalf("correlation was dropped: %+v", batch)
		}
	}
}

func TestToolHistoryRejectsIncompleteCalls(t *testing.T) {
	_, _, err := EncodeToolHistory(ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{{Calls: []ToolHistoryCall{{Status: "succeeded"}}}}})
	if err == nil {
		t.Fatal("incomplete tool call was accepted")
	}
}
