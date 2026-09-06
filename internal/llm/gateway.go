package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

const (
	defaultAsyncPollInterval = time.Second
	maxAsyncPollFailures     = 3
)

// ChatHTTPError describes a non-success response from the chat completion endpoint.
type ChatHTTPError struct {
	StatusCode int
	// Body is bounded request-local provider text used only for narrow retry
	// classification. Error intentionally excludes it because providers may
	// reflect prompt content in error responses.
	Body string
}

func (e *ChatHTTPError) Error() string {
	return fmt.Sprintf("LLM gateway chat returned HTTP %d", e.StatusCode)
}

// IsPermanentChatProviderError reports request failures that should not be retried unchanged.
func IsPermanentChatProviderError(err error) bool {
	var httpErr *ChatHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusBadRequest && httpErr.StatusCode < http.StatusInternalServerError && httpErr.StatusCode != http.StatusRequestTimeout && httpErr.StatusCode != http.StatusTooEarly && httpErr.StatusCode != http.StatusTooManyRequests
}

// IsTemporaryOllamaToolParserError identifies a narrow upstream Ollama/Qwen
// tool-parser failure. Remove this workaround when the provider no longer
// turns malformed generated tool markup into HTTP 500 responses.
func IsTemporaryOllamaToolParserError(err error) bool {
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		return false
	}
	body := strings.ToLower(httpErr.Body)
	return strings.Contains(body, "expected element type") ||
		strings.Contains(body, "xml syntax error") ||
		(strings.Contains(body, "xml") && strings.Contains(body, "unexpected eof"))
}

// IsOllamaModelRunnerStoppedError identifies the upstream failure returned when
// Ollama stops a model runner while processing a request.
func IsOllamaModelRunnerStoppedError(err error) bool {
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		return false
	}
	return strings.Contains(strings.ToLower(httpErr.Body), "model runner has unexpectedly stopped")
}

// IsContextLengthExceededError identifies provider rejections caused by a
// request exceeding the active model context window.
func IsContextLengthExceededError(err error) bool {
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	body := strings.ToLower(httpErr.Body)
	return strings.Contains(body, "context size") ||
		strings.Contains(body, "context length") ||
		strings.Contains(body, "context window") ||
		strings.Contains(body, "too many tokens")
}

// GatewayClient interacts with the LLM gateway's OpenAI-compatible REST API.
type GatewayClient struct {
	BaseURL      string
	APIKey       string
	VirtualKey   string
	HTTPClient   *http.Client
	pollInterval time.Duration
	log          *config.Logger
}

// NewGatewayClient creates an LLM gateway client with the given base URL and optional auth.
func NewGatewayClient(baseURL, apiKey, virtualKey string, log *config.Logger) *GatewayClient {
	return &GatewayClient{
		BaseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:       strings.TrimSpace(apiKey),
		VirtualKey:   strings.TrimSpace(virtualKey),
		HTTPClient:   &http.Client{},
		pollInterval: defaultAsyncPollInterval,
		log:          log,
	}
}

func (c *GatewayClient) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.VirtualKey != "" {
		req.Header.Set("x-bf-vk", c.VirtualKey)
	}
}

func encodeToolArguments(args map[string]interface{}) string {
	if len(args) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func decodeToolArguments(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]interface{}{}
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return map[string]interface{}{"_raw": raw}
	}
	return args
}

func mapToGatewayMessages(msgs []ChatMessage) []gatewayMessage {
	result := make([]gatewayMessage, len(msgs))
	for i, m := range msgs {
		bm := gatewayMessage{Role: m.Role}
		if m.Role == "tool" {
			bm.ToolCallID = m.ToolCallID
			bm.Content = m.Content
			result[i] = bm
			continue
		}
		if len(m.Images) > 0 {
			parts := make([]gatewayContentPart, 0, 1+len(m.Images))
			if strings.TrimSpace(m.Content) != "" {
				parts = append(parts, gatewayContentPart{Type: "text", Text: m.Content})
			}
			for _, image := range m.Images {
				mimeType := strings.TrimSpace(image.MimeType)
				if mimeType == "" {
					mimeType = "image/jpeg"
				}
				parts = append(parts, gatewayContentPart{Type: "image_url", ImageURL: &gatewayImageURL{URL: fmt.Sprintf("data:%s;base64,%s", mimeType, image.Data)}})
			}
			bm.Content = parts
		} else {
			bm.Content = m.Content
		}
		if len(m.ToolCalls) > 0 {
			bm.ToolCalls = make([]gatewayToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				bm.ToolCalls[j] = gatewayToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: gatewayToolFunction{
						Name:      tc.Function.Name,
						Arguments: encodeToolArguments(tc.Function.Arguments),
					},
				}
			}
		}
		result[i] = bm
	}
	return result
}

func contentToString(content interface{}) string {
	switch value := content.(type) {
	case string:
		return value
	case []interface{}:
		var parts []string
		for _, raw := range value {
			obj, ok := raw.(map[string]interface{})
			if !ok || obj["type"] != "text" {
				continue
			}
			if text, ok := obj["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

func mapFromGatewayMessage(m gatewayMessage) ChatMessage {
	msg := ChatMessage{
		Role:       m.Role,
		Content:    contentToString(m.Content),
		Thinking:   firstNonEmpty(m.ReasoningContent, m.Thinking, m.Reasoning),
		ToolCallID: m.ToolCallID,
	}
	if len(m.ToolCalls) > 0 {
		msg.ToolCalls = make([]ToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			msg.ToolCalls[i] = ToolCall{
				ID: tc.ID,
				Function: ToolFunction{
					Name:         tc.Function.Name,
					Arguments:    decodeToolArguments(tc.Function.Arguments),
					RawArguments: tc.Function.Arguments,
				},
			}
		}
	}
	return msg
}

func responseFormat(format string) *gatewayResponseFormat {
	format = strings.TrimSpace(format)
	if format == "" {
		return nil
	}
	return &gatewayResponseFormat{Type: format}
}

func bodySnippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	const maxBodySnippetChars = 500
	if len(text) > maxBodySnippetChars {
		text = text[:maxBodySnippetChars]
	}
	return text
}

func firstChoice(resp gatewayChatResponse) (gatewayChoice, bool) {
	if len(resp.Choices) == 0 {
		return gatewayChoice{}, false
	}
	return resp.Choices[0], true
}

type asyncHTTPError struct {
	StatusCode int
	Body       string
}

func (e *asyncHTTPError) Error() string {
	return fmt.Sprintf("LLM gateway async request returned HTTP %d", e.StatusCode)
}

func (c *GatewayClient) doAsyncRequest(ctx context.Context, method, endpoint string, body []byte) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("create LLM gateway async request: %w", err)
	}
	c.applyHeaders(req)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("send LLM gateway async request: %w", err)
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read LLM gateway async response: %w", err)
	}
	return resp, rawBody, nil
}

func decodeAsyncJob(rawBody []byte) (gatewayAsyncJob, error) {
	var job gatewayAsyncJob
	if err := json.Unmarshal(rawBody, &job); err != nil {
		return gatewayAsyncJob{}, fmt.Errorf("decode LLM gateway async response: %w", err)
	}
	job.ID = strings.TrimSpace(job.ID)
	job.Status = strings.TrimSpace(job.Status)
	return job, nil
}

func (c *GatewayClient) runAsync(ctx context.Context, submitPath string, payload []byte) (json.RawMessage, int, error) {
	submitEndpoint := c.BaseURL + submitPath
	resp, rawBody, err := c.doAsyncRequest(ctx, http.MethodPost, submitEndpoint, payload)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil, 0, &asyncHTTPError{StatusCode: resp.StatusCode, Body: bodySnippet(rawBody)}
	}
	job, err := decodeAsyncJob(rawBody)
	if err != nil {
		return nil, 0, err
	}
	if job.ID == "" || (job.Status != "pending" && job.Status != "processing") {
		return nil, 0, fmt.Errorf("LLM gateway async submission returned invalid job state")
	}

	pollEndpoint := submitEndpoint + "/" + url.PathEscape(job.ID)
	pollFailures := 0
	for {
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, 0, ctx.Err()
		case <-timer.C:
		}

		pollResp, pollBody, pollErr := c.doAsyncRequest(ctx, http.MethodGet, pollEndpoint, nil)
		if pollErr != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			pollFailures++
			if pollFailures >= maxAsyncPollFailures {
				return nil, 0, fmt.Errorf("poll LLM gateway async job: %w", pollErr)
			}
			continue
		}
		if pollResp.StatusCode >= http.StatusInternalServerError {
			pollFailures++
			if pollFailures >= maxAsyncPollFailures {
				return nil, 0, &asyncHTTPError{StatusCode: pollResp.StatusCode, Body: bodySnippet(pollBody)}
			}
			continue
		}
		pollFailures = 0
		if pollResp.StatusCode != http.StatusAccepted && pollResp.StatusCode != http.StatusOK {
			return nil, 0, &asyncHTTPError{StatusCode: pollResp.StatusCode, Body: bodySnippet(pollBody)}
		}
		polled, err := decodeAsyncJob(pollBody)
		if err != nil {
			return nil, 0, err
		}
		if polled.ID != job.ID {
			return nil, 0, fmt.Errorf("LLM gateway async poll returned mismatched job ID")
		}
		switch polled.Status {
		case "pending", "processing":
			if pollResp.StatusCode != http.StatusAccepted {
				return nil, 0, fmt.Errorf("LLM gateway async poll returned non-terminal status with HTTP %d", pollResp.StatusCode)
			}
			continue
		case "completed":
			if pollResp.StatusCode != http.StatusOK || len(polled.Result) == 0 || string(polled.Result) == "null" {
				return nil, 0, fmt.Errorf("LLM gateway async job completed without a result")
			}
			return polled.Result, polled.StatusCode, nil
		case "failed":
			if pollResp.StatusCode != http.StatusOK {
				return nil, 0, fmt.Errorf("LLM gateway async job failed with invalid HTTP status")
			}
			statusCode := polled.StatusCode
			if statusCode < 100 || statusCode > 599 {
				statusCode = http.StatusInternalServerError
			}
			return nil, 0, &asyncHTTPError{StatusCode: statusCode, Body: bodySnippet(polled.Error)}
		default:
			return nil, 0, fmt.Errorf("LLM gateway async poll returned unknown status")
		}
	}
}

// Embed submits text through Bifrost's async embeddings endpoint and polls for vectors.
func (c *GatewayClient) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	payloadBytes, err := json.Marshal(gatewayEmbeddingRequest{Model: req.Model, Input: req.Input})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embedding request: %w", err)
	}
	rawBody, _, err := c.runAsync(ctx, "/v1/async/embeddings", payloadBytes)
	requestLog := c.requestLog(ctx, req.Model)
	if err != nil {
		var httpErr *asyncHTTPError
		if errors.As(err, &httpErr) {
			requestLog.Error("provider.gateway.embed.http_error", "LLM gateway async embed failed", config.F("operation", "embed"), config.F("http_status", httpErr.StatusCode), config.F("status", "error"))
			return nil, fmt.Errorf("LLM gateway embed returned HTTP %d", httpErr.StatusCode)
		}
		return nil, fmt.Errorf("LLM gateway async embed failed: %w", err)
	}

	var gatewayResp gatewayEmbeddingResponse
	if err := json.Unmarshal(rawBody, &gatewayResp); err != nil {
		requestLog.Error("provider.gateway.embed.decode_error", "failed to decode LLM gateway embed response", config.F("operation", "embed"), config.ErrorField(err))
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}
	if gatewayResp.Error != nil {
		requestLog.Error("provider.gateway.embed.response_error", "LLM gateway embed response reported an error", config.F("operation", "embed"), config.F("status", "error"))
		return nil, fmt.Errorf("LLM gateway embed response reported an error")
	}
	if len(gatewayResp.Data) == 0 || len(gatewayResp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("LLM gateway embed response contained no embeddings")
	}

	embeddings := make([][]float64, 0, len(gatewayResp.Data))
	for _, datum := range gatewayResp.Data {
		embeddings = append(embeddings, datum.Embedding)
	}
	return &EmbedResponse{Model: gatewayResp.Model, Embeddings: embeddings}, nil
}

// Chat streams synchronous requests and polls Bifrost async jobs for non-streaming requests.
func (c *GatewayClient) Chat(ctx context.Context, req ChatRequest, chatStreamCallback func(chunk ChatMessage)) (*ChatResponse, error) {
	gatewayReq := gatewayChatRequest{Model: req.Model, User: req.User, Messages: mapToGatewayMessages(req.Messages), Tools: req.Tools, ToolChoice: req.ToolChoice, ParallelToolCalls: req.ParallelToolCalls, Temperature: req.Temperature, MaxTokens: req.MaxTokens, ResponseFormat: responseFormat(req.Format), Stream: req.Stream}
	payloadBytes, err := json.Marshal(gatewayReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat request: %w", err)
	}

	startedAt := time.Now()
	if req.Stream {
		endpoint := fmt.Sprintf("%s/v1/chat/completions", c.BaseURL)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create chat stream request: %w", err)
		}
		c.applyHeaders(httpReq)
		resp, err := c.HTTPClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("LLM gateway chat stream request failed: %w", err)
		}
		defer resp.Body.Close()
		if chatStreamCallback == nil {
			chatStreamCallback = func(ChatMessage) {}
		}
		chatResp, streamErr := c.readChatStream(ctx, resp, req.Model, startedAt, chatStreamCallback)
		if streamErr != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return chatResp, streamErr
	}
	rawBody, _, err := c.runAsync(ctx, "/v1/async/chat/completions", payloadBytes)
	if err != nil {
		var httpErr *asyncHTTPError
		if errors.As(err, &httpErr) {
			return nil, &ChatHTTPError{StatusCode: httpErr.StatusCode, Body: httpErr.Body}
		}
		return nil, fmt.Errorf("LLM gateway async chat failed: %w", err)
	}
	return c.decodeChatResponse(ctx, rawBody, req.Model, startedAt)
}

func (c *GatewayClient) requestLog(ctx context.Context, model string) *config.Logger {
	meta := requestctx.MetadataFromContext(ctx)
	principal, _ := requestctx.PrincipalFromContext(ctx)
	return c.log.Server("provider.gateway",
		config.F("request_id", meta.RequestID),
		config.F("gateway", principal.Gateway),
		config.F("user_id", principal.CanonicalUserID),
		config.F("session_id", meta.SessionID),
		config.F("model", model),
	)
}

func (c *GatewayClient) decodeChatResponse(ctx context.Context, rawBody []byte, model string, startedAt time.Time) (*ChatResponse, error) {
	requestLog := c.requestLog(ctx, model)
	var gatewayResp gatewayChatResponse
	if err := json.Unmarshal(rawBody, &gatewayResp); err != nil {
		requestLog.Error("provider.gateway.chat.decode_error", "failed to decode LLM gateway chat response", config.F("operation", "chat"), config.ErrorField(err))
		return nil, fmt.Errorf("failed to decode chat response: %w", err)
	}
	if gatewayResp.Error != nil {
		requestLog.Error("provider.gateway.chat.response_error", "LLM gateway chat response reported an error", config.F("operation", "chat"), config.F("status", "error"))
		return nil, fmt.Errorf("LLM gateway chat response reported an error")
	}
	choice, ok := firstChoice(gatewayResp)
	if !ok {
		return nil, fmt.Errorf("LLM gateway chat response contained no choices")
	}
	msg := mapFromGatewayMessage(choice.Message)
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	durationMS := time.Since(startedAt).Milliseconds()
	requestLog.Info("provider.gateway.chat.complete", "LLM gateway chat completed",
		config.F("operation", "chat"),
		config.F("duration_ms", durationMS),
		config.F("prompt_tokens", gatewayResp.Usage.PromptTokens),
		config.F("completion_tokens", gatewayResp.Usage.CompletionTokens),
		config.F("total_tokens", gatewayResp.Usage.TotalTokens),
		config.F("is_usage_reported", usageReported(gatewayResp.Usage.PromptTokens, gatewayResp.Usage.CompletionTokens, gatewayResp.Usage.TotalTokens)),
		config.F("done_reason", choice.FinishReason),
		config.F("is_streaming", false),
		config.F("status", "ok"),
	)

	return &ChatResponse{
		Model:            firstNonEmpty(gatewayResp.Model, model),
		Message:          msg,
		PromptTokens:     gatewayResp.Usage.PromptTokens,
		CompletionTokens: gatewayResp.Usage.CompletionTokens,
		TotalTokens:      gatewayResp.Usage.TotalTokens,
		DurationMS:       durationMS,
		DoneReason:       choice.FinishReason,
	}, nil
}

func (c *GatewayClient) readChatStream(ctx context.Context, resp *http.Response, model string, startedAt time.Time, chatStreamCallback func(chunk ChatMessage)) (*ChatResponse, error) {
	requestLog := c.requestLog(ctx, model)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read chat stream response body: %w", readErr)
		}
		snippet := bodySnippet(rawBody)
		requestLog.Error("provider.gateway.chat.http_error", "LLM gateway chat stream returned non-2xx", config.F("operation", "chat_stream"), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(rawBody)), config.F("status", "error"))
		return nil, &ChatHTTPError{StatusCode: resp.StatusCode, Body: snippet}
	}

	var final ChatResponse
	final.Model = model
	toolParts := map[int]*gatewayStreamToolCall{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk gatewayChatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			requestLog.Warn("provider.gateway.chat.stream.parse_failed", "failed to parse LLM gateway chat stream chunk", config.F("operation", "chat_stream"), config.F("status", "degraded"), config.ErrorField(err))
			continue
		}
		if chunk.Error != nil {
			requestLog.Error("provider.gateway.chat.response_error", "LLM gateway chat stream reported an error", config.F("operation", "chat_stream"), config.F("status", "error"))
			return nil, fmt.Errorf("LLM gateway chat stream reported an error")
		}
		if chunk.Model != "" {
			final.Model = chunk.Model
		}
		if usageReported(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens) {
			final.PromptTokens = chunk.Usage.PromptTokens
			final.CompletionTokens = chunk.Usage.CompletionTokens
			final.TotalTokens = chunk.Usage.TotalTokens
		}
		choice, ok := firstChoice(chunk)
		if !ok {
			continue
		}
		final.DoneReason = choice.FinishReason
		content := contentToString(choice.Delta.Content)
		thinking := firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Thinking, choice.Delta.Reasoning)
		if thinking != "" {
			final.Message.Role = "assistant"
			final.Message.Thinking += thinking
			chatStreamCallback(ChatMessage{Role: "assistant", Thinking: thinking})
		}
		if content != "" {
			final.Message.Role = "assistant"
			final.Message.Content += content
			chatStreamCallback(ChatMessage{Role: "assistant", Content: content})
		}
		for _, tc := range choice.Delta.ToolCalls {
			idx := tc.Index
			part, ok := toolParts[idx]
			if !ok {
				part = &gatewayStreamToolCall{}
				toolParts[idx] = part
			}
			if tc.ID != "" {
				part.ID = tc.ID
			}
			if tc.Function.Name != "" {
				part.Name = tc.Function.Name
			}
			part.Arguments += tc.Function.Arguments
		}
	}
	if err := scanner.Err(); err != nil {
		requestLog.Warn("provider.gateway.chat.stream.scan_failed", "LLM gateway chat stream scan failed", config.F("operation", "chat_stream"), config.F("status", "error"), config.ErrorField(err))
		return nil, fmt.Errorf("read LLM gateway chat stream: %w", err)
	}

	if len(toolParts) > 0 {
		final.Message.Role = "assistant"
		indices := make([]int, 0, len(toolParts))
		for i := range toolParts {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			part := toolParts[i]
			if part == nil || part.Name == "" {
				continue
			}
			final.Message.ToolCalls = append(final.Message.ToolCalls, ToolCall{ID: firstNonEmpty(part.ID, fmt.Sprintf("call_stream_%d", i+1)), Function: ToolFunction{Name: part.Name, Arguments: decodeToolArguments(part.Arguments), RawArguments: part.Arguments}})
		}
	}
	final.DurationMS = time.Since(startedAt).Milliseconds()
	requestLog.Info("provider.gateway.chat.complete", "LLM gateway chat completed",
		config.F("operation", "chat_stream"),
		config.F("duration_ms", final.DurationMS),
		config.F("prompt_tokens", final.PromptTokens),
		config.F("completion_tokens", final.CompletionTokens),
		config.F("total_tokens", final.TotalTokens),
		config.F("is_usage_reported", usageReported(final.PromptTokens, final.CompletionTokens, final.TotalTokens)),
		config.F("done_reason", final.DoneReason),
		config.F("is_streaming", true),
		config.F("status", "ok"),
	)
	return &final, nil
}

func usageReported(promptTokens, completionTokens, totalTokens int) bool {
	return promptTokens > 0 || completionTokens > 0 || totalTokens > 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
