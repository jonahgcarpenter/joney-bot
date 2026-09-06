package llm

import "encoding/json"

type gatewayToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type gatewayToolCall struct {
	ID       string              `json:"id,omitempty"`
	Index    int                 `json:"index,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function gatewayToolFunction `json:"function"`
}

type gatewayImageURL struct {
	URL string `json:"url"`
}

type gatewayContentPart struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *gatewayImageURL `json:"image_url,omitempty"`
}

type gatewayMessage struct {
	Role             string            `json:"role"`
	Content          interface{}       `json:"content,omitempty"`
	Reasoning        string            `json:"reasoning,omitempty"`
	Thinking         string            `json:"thinking,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []gatewayToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
}

type gatewayResponseFormat struct {
	Type string `json:"type"`
}

type gatewayChatRequest struct {
	Model             string                 `json:"model"`
	User              string                 `json:"user,omitempty"`
	Messages          []gatewayMessage       `json:"messages"`
	Tools             []Tool                 `json:"tools,omitempty"`
	ToolChoice        ToolChoice             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                  `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64               `json:"temperature,omitempty"`
	MaxTokens         int                    `json:"max_tokens,omitempty"`
	ResponseFormat    *gatewayResponseFormat `json:"response_format,omitempty"`
	Stream            bool                   `json:"stream"`
}

type gatewayChatResponse struct {
	ID      string                `json:"id,omitempty"`
	Model   string                `json:"model,omitempty"`
	Choices []gatewayChoice       `json:"choices"`
	Usage   gatewayUsage          `json:"usage,omitempty"`
	Error   *gatewayErrorResponse `json:"error,omitempty"`
}

type gatewayChoice struct {
	Index        int            `json:"index,omitempty"`
	Message      gatewayMessage `json:"message,omitempty"`
	Delta        gatewayMessage `json:"delta,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
}

type gatewayUsage struct {
	reported                                          bool
	promptReported, completionReported, totalReported bool
	PromptTokens                                      int `json:"prompt_tokens,omitempty"`
	CompletionTokens                                  int `json:"completion_tokens,omitempty"`
	TotalTokens                                       int `json:"total_tokens,omitempty"`
}

func (u *gatewayUsage) UnmarshalJSON(data []byte) error {
	type wireUsage gatewayUsage
	var value wireUsage
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*u = gatewayUsage(value)
	for key, reported := range map[string]*bool{"prompt_tokens": &u.promptReported, "completion_tokens": &u.completionReported, "total_tokens": &u.totalReported} {
		if raw, ok := fields[key]; ok && string(raw) != "null" {
			*reported = true
			u.reported = true
		}
	}
	return nil
}

type gatewayErrorResponse struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

type gatewayStreamToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type gatewayEmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type gatewayEmbeddingResponse struct {
	Usage gatewayUsage            `json:"usage,omitempty"`
	Model string                  `json:"model,omitempty"`
	Data  []gatewayEmbeddingDatum `json:"data"`
	Error *gatewayErrorResponse   `json:"error,omitempty"`
}

type gatewayEmbeddingDatum struct {
	Embedding []float64 `json:"embedding"`
}

type gatewayAsyncJob struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	StatusCode int             `json:"status_code,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      json.RawMessage `json:"error,omitempty"`
}
