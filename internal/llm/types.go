package llm

import "context"

// ToolFunction holds the name and arguments of a single tool invocation.
type ToolFunction struct {
	Name         string                 `json:"name"`
	Arguments    map[string]interface{} `json:"arguments"`
	RawArguments string                 `json:"-"`
}

// ToolCall represents a single tool call emitted by the model.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Function ToolFunction `json:"function"`
}

// InputImage is a validated image payload attached to a user request.
// Data must contain base64-encoded normalized image bytes.
type InputImage struct {
	MimeType          string `json:"mime_type,omitempty"`
	Data              string `json:"data"`
	Source            string `json:"source,omitempty"`
	IsGIFContactSheet bool   `json:"-"`
}

// ChatMessage is a single turn in a conversation.
type ChatMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	Images     []InputImage `json:"images,omitempty"`
	Thinking   string       `json:"thinking,omitempty"`
	ToolCalls  []ToolCall   `json:"tool_calls,omitempty"`
	ToolName   string       `json:"tool_name,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

// ToolParameterProperty describes a single property within a tool's parameter schema.
type ToolParameterProperty struct {
	Type                 string                           `json:"type"`
	Description          string                           `json:"description,omitempty"`
	Enum                 []string                         `json:"enum,omitempty"`
	Properties           map[string]ToolParameterProperty `json:"properties,omitempty"`
	Required             []string                         `json:"required,omitempty"`
	Items                *ToolParameterProperty           `json:"items,omitempty"`
	MinItems             *int                             `json:"minItems,omitempty"`
	MaxItems             *int                             `json:"maxItems,omitempty"`
	Minimum              *float64                         `json:"minimum,omitempty"`
	Maximum              *float64                         `json:"maximum,omitempty"`
	MinLength            *int                             `json:"minLength,omitempty"`
	MaxLength            *int                             `json:"maxLength,omitempty"`
	AdditionalProperties *bool                            `json:"additionalProperties,omitempty"`
}

// ToolParameters is the JSON Schema object describing a tool's input parameters.
type ToolParameters struct {
	Type                 string                           `json:"type"`
	Properties           map[string]ToolParameterProperty `json:"properties"`
	Required             []string                         `json:"required,omitempty"`
	AdditionalProperties *bool                            `json:"additionalProperties,omitempty"`
}

// ToolDefinition holds the schema for a single function tool.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  ToolParameters `json:"parameters"`
}

// Tool wraps a ToolDefinition with its type identifier (always "function").
type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

// ToolChoice controls whether the provider may choose or must call a tool.
type ToolChoice string

const (
	// ToolChoiceRequired requires a tool call. Callers that need one specific
	// tool must advertise only that tool and disable parallel tool calls.
	ToolChoiceRequired ToolChoice = "required"
)

// ChatRequest is the provider-neutral payload for a chat-style LLM request.
type ChatRequest struct {
	Model             string        `json:"model"`
	User              string        `json:"user,omitempty"`
	Messages          []ChatMessage `json:"messages"`
	Tools             []Tool        `json:"tools,omitempty"`
	ToolChoice        ToolChoice    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64      `json:"temperature,omitempty"`
	MaxTokens         int           `json:"max_tokens,omitempty"`
	Format            string        `json:"format,omitempty"`
	Stream            bool          `json:"stream"`
}

// ChatResponse is the standardized reply from a chat LLM call.
type ChatResponse struct {
	Model            string
	Message          ChatMessage
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMS       int64
	DoneReason       string
}

// EmbedRequest is the provider-neutral payload for an embedding request.
type EmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// EmbedResponse contains vectors returned by an embedding endpoint.
type EmbedResponse struct {
	Model      string
	Embeddings [][]float64
}

// Chatter describes the chat capability the agent depends on.
type Chatter interface {
	Chat(ctx context.Context, req ChatRequest, chatStreamCallback func(chunk ChatMessage)) (*ChatResponse, error)
}

// Embedder describes the embedding capability used for semantic memory retrieval.
type Embedder interface {
	Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error)
}
