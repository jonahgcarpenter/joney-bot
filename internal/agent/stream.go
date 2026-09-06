package agent

import (
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/webfetch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

// StreamChunkType identifies the kind of content in a StreamChunk.
type StreamChunkType string

const (
	// ChunkThinking carries tokens from the model's internal reasoning phase.
	ChunkThinking StreamChunkType = "thinking"

	// ChunkContent carries tokens from the model's visible response.
	ChunkContent StreamChunkType = "content"

	// ChunkStatus carries status messages injected by the agent (e.g. "[Calling: web.search]").
	ChunkStatus StreamChunkType = "status"

	// ChunkToolCall carries structured tool invocation data for frontend timelines.
	ChunkToolCall StreamChunkType = "tool_call"

	// ChunkToolResult carries structured tool result data for frontend timelines.
	ChunkToolResult StreamChunkType = "tool_result"
)

// ToolStreamSearchResult is a UI-safe search result emitted for web.search tools.
type ToolStreamSearchResult struct {
	Title       string   `json:"title,omitempty"`
	URL         string   `json:"url,omitempty"`
	Domain      string   `json:"domain,omitempty"`
	Content     string   `json:"content,omitempty"`
	Engines     []string `json:"engines,omitempty"`
	PublishedAt string   `json:"published_at,omitempty"`
	Score       float64  `json:"score,omitempty"`
}

// ToolStreamSearchPayload contains structured web.search details for streaming UIs.
type ToolStreamSearchPayload struct {
	Query               string                   `json:"query,omitempty"`
	Results             []ToolStreamSearchResult `json:"results,omitempty"`
	IsDegraded          bool                     `json:"is_degraded,omitempty"`
	UnresponsiveEngines []string                 `json:"unresponsive_engines,omitempty"`
}

// ToolStreamFetchPayload contains privacy-safe web.fetch details for streaming UIs.
type ToolStreamFetchPayload struct {
	Title       string `json:"title,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Source      string `json:"source,omitempty"`
	IsTruncated bool   `json:"is_truncated,omitempty"`
	IsDegraded  bool   `json:"is_degraded,omitempty"`
}

// ToolStreamPayload contains structured tool data for frontend rendering.
type ToolStreamPayload struct {
	Name         string                         `json:"name"`
	Arguments    map[string]interface{}         `json:"arguments,omitempty"`
	ResultText   string                         `json:"result_text,omitempty"`
	DurationMS   int64                          `json:"duration_ms,omitempty"`
	IsError      bool                           `json:"is_error,omitempty"`
	WebSearch    *ToolStreamSearchPayload       `json:"web.search,omitempty"`
	WebFetch     *ToolStreamFetchPayload        `json:"web.fetch,omitempty"`
	UserMemory   *ToolStreamUserMemoryPayload   `json:"user_memory,omitempty"`
	GlobalMemory *ToolStreamGlobalMemoryPayload `json:"global_memory,omitempty"`
}

// ToolStreamUserMemoryPayload contains structured user-memory tool details.
type ToolStreamUserMemoryPayload struct {
	Action   string                   `json:"action,omitempty"`
	Category string                   `json:"category,omitempty"`
	Content  *memory.RenderedSections `json:"content,omitempty"`
}

// ToolStreamGlobalMemoryPayload contains structured global-memory tool details.
type ToolStreamGlobalMemoryPayload struct {
	Action string `json:"action,omitempty"`
	Query  string `json:"query,omitempty"`
}

// StreamChunk is a single typed token event streamed to gateways during Process().
// Gateways receive thinking tokens, content tokens, and agent status messages via this type.
type StreamChunk struct {
	Type        StreamChunkType          `json:"type"`
	Text        string                   `json:"text,omitempty"`
	Tool        *ToolStreamPayload       `json:"tool,omitempty"`
	Attachments []media.OutputAttachment `json:"-"`
}

func toolStreamPayload(toolName string, args map[string]interface{}, result string, duration time.Duration, isError bool) *ToolStreamPayload {
	payload := &ToolStreamPayload{
		Name:       toolName,
		Arguments:  args,
		ResultText: result,
		DurationMS: duration.Milliseconds(),
		IsError:    isError,
	}
	if toolName == toolnames.UserMemorySave {
		payload.Arguments = nil
		payload.ResultText = ""
		payload.UserMemory = &ToolStreamUserMemoryPayload{Action: "save"}
		return payload
	}
	if toolName == toolnames.ComfyUITextToImage || toolName == toolnames.ComfyUIImageToImage {
		payload.Arguments = nil
		payload.ResultText = ""
		return payload
	}
	if toolName == toolnames.WebFetch {
		payload.Arguments = nil
		payload.ResultText = ""
		fetchPayload := &ToolStreamFetchPayload{}
		if !isError && result != "" {
			if response, err := webfetch.DecodeToolResponse(result); err == nil {
				fetchPayload.Title = response.Title
				fetchPayload.ContentType = response.ContentType
				fetchPayload.Source = response.Source
				fetchPayload.IsTruncated = response.IsTruncated
				fetchPayload.IsDegraded = response.IsDegraded
			}
		}
		payload.WebFetch = fetchPayload
		return payload
	}

	if toolName != toolnames.WebSearch {
		switch toolName {
		case toolnames.UserMemorySearch, toolnames.UserMemoryList:
			payload.UserMemory = userMemoryStreamPayload(toolName, args, result, isError)
		case toolnames.GlobalMemorySearch:
			payload.GlobalMemory = globalMemoryStreamPayload(args)
		}
		return payload
	}

	searchPayload := &ToolStreamSearchPayload{}
	if query, ok := args["query"].(string); ok {
		searchPayload.Query = strings.TrimSpace(query)
	}
	if !isError {
		response, err := websearch.DecodeToolResponse(result)
		if err == nil {
			searchPayload.IsDegraded = response.Degraded
			searchPayload.UnresponsiveEngines = response.UnresponsiveEngines
			searchPayload.Results = make([]ToolStreamSearchResult, 0, len(response.Results))
			for _, r := range response.Results {
				searchPayload.Results = append(searchPayload.Results, ToolStreamSearchResult{
					Title:       r.Title,
					URL:         r.URL,
					Domain:      r.Domain,
					Content:     r.Snippet,
					Engines:     r.Engines,
					PublishedAt: r.PublishedAt,
					Score:       r.Score,
				})
			}
		}
	}
	payload.WebSearch = searchPayload
	return payload
}

func userMemoryStreamPayload(toolName string, args map[string]interface{}, result string, isError bool) *ToolStreamUserMemoryPayload {
	payload := &ToolStreamUserMemoryPayload{Action: userMemoryToolAction(toolName)}
	if category, ok := args["category"].(string); ok {
		payload.Category = strings.TrimSpace(strings.ToLower(category))
	}
	if isError {
		return payload
	}
	if payload.Action == "search" || payload.Action == "list" {
		content := memory.ParseRenderedMarkdown(result)
		if content.Intro != "" || len(content.Sections) > 0 {
			payload.Content = &content
		}
	}
	return payload
}

func userMemoryToolAction(toolName string) string {
	switch toolName {
	case toolnames.UserMemorySave:
		return "save"
	case toolnames.UserMemorySearch:
		return "search"
	case toolnames.UserMemoryList:
		return "list"
	}
	return ""
}

func globalMemoryStreamPayload(args map[string]interface{}) *ToolStreamGlobalMemoryPayload {
	payload := &ToolStreamGlobalMemoryPayload{Action: "search"}
	if query, ok := args["query"].(string); ok {
		payload.Query = strings.TrimSpace(query)
	}
	return payload
}
