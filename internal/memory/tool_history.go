package memory

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	ToolHistoryVersion       = 1
	MaxToolHistoryBytes      = 64 * 1024
	MaxToolSearchTextRunes   = 12_000
	toolHistoryOmittedResult = "Historical tool result omitted because the durable history limit was reached."
)

// ToolHistory is the immutable native tool-call trace for one session turn.
type ToolHistory struct {
	Version int                `json:"version"`
	Batches []ToolHistoryBatch `json:"batches"`
}

// ToolHistoryBatch preserves one assistant tool-call iteration and its call order.
type ToolHistoryBatch struct {
	AssistantContent string            `json:"assistant_content,omitempty"`
	Calls            []ToolHistoryCall `json:"calls"`
}

// ToolHistoryCall preserves one call and its exactly correlated result.
type ToolHistoryCall struct {
	Name               string                 `json:"name"`
	HistoryMode        string                 `json:"history_mode,omitempty"`
	Arguments          map[string]interface{} `json:"arguments,omitempty"`
	Status             string                 `json:"status"`
	Outcome            string                 `json:"outcome,omitempty"`
	ReasonCode         string                 `json:"reason_code,omitempty"`
	IsDegraded         bool                   `json:"is_degraded,omitempty"`
	Result             string                 `json:"result"`
	ExecutedAt         string                 `json:"executed_at"`
	ArgumentsTruncated bool                   `json:"arguments_truncated,omitempty"`
	ResultTruncated    bool                   `json:"result_truncated,omitempty"`
	SearchResult       bool                   `json:"search_result,omitempty"`
}

// EmptyToolHistory returns the canonical empty trace.
func EmptyToolHistory() ToolHistory {
	return ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{}}
}

// EncodeToolHistory validates and bounds a trace for atomic session storage.
func EncodeToolHistory(history ToolHistory) (string, string, error) {
	if history.Version == 0 {
		history.Version = ToolHistoryVersion
	}
	if history.Version != ToolHistoryVersion {
		return "", "", fmt.Errorf("unsupported tool history version %d", history.Version)
	}
	if history.Batches == nil {
		history.Batches = []ToolHistoryBatch{}
	}
	for batchIndex := range history.Batches {
		batch := &history.Batches[batchIndex]
		if len(batch.Calls) == 0 {
			return "", "", fmt.Errorf("tool history batch %d has no calls", batchIndex)
		}
		for callIndex := range batch.Calls {
			call := &batch.Calls[callIndex]
			call.Name = strings.TrimSpace(call.Name)
			if call.Name == "" || strings.TrimSpace(call.Status) == "" {
				return "", "", fmt.Errorf("tool history call %d:%d is incomplete", batchIndex, callIndex)
			}
			if call.Arguments == nil {
				call.Arguments = map[string]interface{}{}
			}
		}
	}
	encoded, err := json.Marshal(history)
	if err != nil {
		return "", "", fmt.Errorf("encode tool history: %w", err)
	}
	if len(encoded) > MaxToolHistoryBytes {
		boundToolHistory(&history)
		encoded, err = json.Marshal(history)
		if err != nil {
			return "", "", fmt.Errorf("encode bounded tool history: %w", err)
		}
	}
	if len(encoded) > MaxToolHistoryBytes {
		return "", "", fmt.Errorf("tool history exceeds %d bytes", MaxToolHistoryBytes)
	}
	return string(encoded), ToolHistorySearchText(history), nil
}

// DecodeToolHistory decodes a canonical trace and rejects unsupported shapes.
func DecodeToolHistory(encoded string) (ToolHistory, error) {
	if strings.TrimSpace(encoded) == "" {
		return EmptyToolHistory(), nil
	}
	var history ToolHistory
	if err := json.Unmarshal([]byte(encoded), &history); err != nil {
		return ToolHistory{}, fmt.Errorf("decode tool history: %w", err)
	}
	if history.Version != ToolHistoryVersion || history.Batches == nil {
		return ToolHistory{}, fmt.Errorf("invalid tool history")
	}
	for batchIndex, batch := range history.Batches {
		if len(batch.Calls) == 0 {
			return ToolHistory{}, fmt.Errorf("tool history batch %d has no calls", batchIndex)
		}
		for callIndex, call := range batch.Calls {
			if strings.TrimSpace(call.Name) == "" || strings.TrimSpace(call.Status) == "" {
				return ToolHistory{}, fmt.Errorf("tool history call %d:%d is incomplete", batchIndex, callIndex)
			}
		}
	}
	return history, nil
}

// ToolHistorySearchText renders the bounded projection used by transcript FTS.
func ToolHistorySearchText(history ToolHistory) string {
	var b strings.Builder
	for _, batch := range history.Batches {
		for _, call := range batch.Calls {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(call.Name)
			if call.SearchResult && strings.TrimSpace(call.Result) != "" {
				b.WriteByte('\n')
				b.WriteString(strings.TrimSpace(call.Result))
			}
		}
	}
	return truncateRunes(strings.TrimSpace(b.String()), MaxToolSearchTextRunes)
}

func boundToolHistory(history *ToolHistory) {
	for batchIndex := range history.Batches {
		content := []rune(history.Batches[batchIndex].AssistantContent)
		if len(content) > 2000 {
			history.Batches[batchIndex].AssistantContent = string(content[:2000])
		}
	}
	for batchIndex := 0; batchIndex < len(history.Batches); batchIndex++ {
		for callIndex := 0; callIndex < len(history.Batches[batchIndex].Calls); callIndex++ {
			call := &history.Batches[batchIndex].Calls[callIndex]
			if call.Result != toolHistoryOmittedResult {
				call.Result = toolHistoryOmittedResult
				call.ResultTruncated = true
			}
			encoded, _ := json.Marshal(history)
			if len(encoded) <= MaxToolHistoryBytes {
				return
			}
			call.Arguments = map[string]interface{}{}
			call.ArgumentsTruncated = true
			encoded, _ = json.Marshal(history)
			if len(encoded) <= MaxToolHistoryBytes {
				return
			}
		}
	}
}

func successfulToolHistoryNames(history ToolHistory) []string {
	var names []string
	for _, batch := range history.Batches {
		for _, call := range batch.Calls {
			if call.Status == "succeeded" {
				names = append(names, strings.TrimSpace(call.Name))
			}
		}
	}
	return uniqueStrings(names)
}

func truncateRunes(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}
