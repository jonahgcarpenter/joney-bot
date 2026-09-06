package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/webfetch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	toolruntime "github.com/jonahgcarpenter/oswald-ai/internal/tools/runtime"
)

const (
	sessionHistoryCandidateLimit = 1000
	recentToolExposureTurns      = 4
	automaticRecallTopK          = 4
	automaticRecallCharLimit     = 2000
	sessionTurnTTL               = 24 * time.Hour
	emptyResponseRetryPrompt     = "Your previous completion contained no visible response. Answer the user's last request now using only visible response content."
	emptyResponseFallback        = "I blanked on the actual answer. Try again and I'll take another shot."
	imageSizeFallback            = "Your image is too big. Crop it and try again."
	contextCompactionFallback    = "I ran out of context while working through that. Try splitting the task into smaller steps."
	maxImageModelAttempts        = 5
	imageRetryScale              = 0.75
	imageInitialScaleMaxEdge     = 1920
	sessionPromptPressurePrefix  = "session-prompt-pressure-v1"
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
	Action   string                    `json:"action,omitempty"`
	Category string                    `json:"category,omitempty"`
	Content  *usermemory.ParsedContent `json:"content,omitempty"`
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
	if toolName == "web.fetch" {
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

	if toolName != "web.search" {
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
		content := usermemory.ParseContent(result)
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

// ModelMetrics holds performance data from a single LLM call.
type ModelMetrics struct {
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	TotalTokens      int     `json:"total_tokens,omitempty"`
	DurationMS       int64   `json:"duration_ms,omitempty"`
	TokensPerSecond  float64 `json:"tokens_per_second"`
}

// AgentResponse is the final payload returned to the gateway after processing.
type AgentResponse struct {
	Model       string                   `json:"model"`
	Response    string                   `json:"response,omitempty"`
	Thinking    string                   `json:"thinking,omitempty"` // reasoning tokens emitted before the response
	Error       string                   `json:"error,omitempty"`
	Metrics     *ModelMetrics            `json:"metrics,omitempty"`
	Attachments []media.OutputAttachment `json:"-"`

	SourceTurnID      int64 `json:"-"`
	SessionGeneration int   `json:"-"`
}

// Request contains one fully resolved request submitted to the agent.
type Request struct {
	RequestID   string
	Principal   identity.Principal
	DisplayName string
	SessionKey  string
	IsDirect    bool
	Prompt      string
	Images      []llm.InputImage
	StreamFunc  func(StreamChunk)
}

// Agent handles LLM orchestration: a single agentic loop where the model
// calls tools from the registry and generates the final response.
type Agent struct {
	chatClient  llm.Chatter
	registry    *registry.Registry
	mcpProvider MCPProvider
	budget      promptbudget.ContextBudget
	model       string
	soul        *soul.Store
	userMemory  *usermemory.Store
	toolPolicy  governance.GlobalPolicy
	compactor   ForegroundCompactor
	log         *config.Logger
}

// SetForegroundCompactor installs the request-local compactor before the agent starts serving work.
func (a *Agent) SetForegroundCompactor(compactor ForegroundCompactor) {
	if a != nil {
		a.compactor = compactor
	}
}

// MCPProvider resolves request-scoped MCP tools for the active canonical user.
type MCPProvider interface {
	DiscoveryTools(ctx context.Context, principal identity.Principal) []llm.Tool
	ResolveTools(ctx context.Context, principal identity.Principal, names []string) []string
	LLMTools(ctx context.Context, principal identity.Principal, exposed map[string]bool) []llm.Tool
	Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposed map[string]bool) (mcp.ExecutionResult, bool, error)
	ToolPolicy(name string) governance.ToolPolicy
}

// NewAgent initializes the Agent with an LLM chat client, tool registry, model name,
// soul store, SQLite user memory store, prompt budget, tool-governance policy,
// and logger.
func NewAgent(
	chatClient llm.Chatter,
	registry *registry.Registry,
	model string,
	soul *soul.Store,
	userMemory *usermemory.Store,
	budget promptbudget.ContextBudget,
	toolPolicy governance.GlobalPolicy,
	log *config.Logger,
	mcpProviders ...MCPProvider,
) *Agent {
	var mcpProvider MCPProvider
	if len(mcpProviders) > 0 {
		mcpProvider = mcpProviders[0]
	}
	return &Agent{
		chatClient:  chatClient,
		registry:    registry,
		mcpProvider: mcpProvider,
		budget:      budget,
		model:       model,
		soul:        soul,
		userMemory:  userMemory,
		toolPolicy:  toolPolicy,
		log:         log,
	}
}

func stripReplyContext(prompt string) (string, bool) {
	prompt = strings.TrimSpace(prompt)
	if !strings.HasPrefix(prompt, "[Replying ") {
		return prompt, false
	}
	parts := strings.SplitN(prompt, "\n\n", 2)
	if len(parts) < 2 {
		return "", true
	}
	return strings.TrimSpace(parts[1]), true
}

func sessionMemoryUserContent(prompt string, imageCount int) string {
	content, hadReplyContext := stripReplyContext(prompt)
	if content == "" && hadReplyContext {
		content = "[User replied to a prior message]"
	}
	if imageCount > 0 {
		content = strings.TrimSpace(content + fmt.Sprintf("\n\n[Attached %d image(s)]", imageCount))
	}
	return strings.TrimSpace(content)
}

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func providerUserValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "You are speaking with ")
	value = strings.TrimSuffix(value, ".")
	return strings.TrimSpace(value)
}

func gatewaySystemPrompt(gateway string) string {
	switch strings.TrimSpace(strings.ToLower(gateway)) {
	case "imessage":
		return "# Gateway Instructions\nThe user is reading this in iMessage, which does not render Markdown. Write responses in plain text. Do not use Markdown formatting such as **bold**, headings, tables, fenced code blocks, or inline code ticks. Use simple line breaks and plain bullets when helpful."
	default:
		return ""
	}
}

// mapMetrics converts an LLM response into a model metrics summary.
func mapMetrics(resp *llm.ChatResponse) *ModelMetrics {
	if resp == nil {
		return nil
	}
	tps := 0.0
	if resp.DurationMS > 0 && resp.CompletionTokens > 0 {
		tps = float64(resp.CompletionTokens) / (float64(resp.DurationMS) / 1000)
	}
	return &ModelMetrics{
		Model:            resp.Model,
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		TotalTokens:      resp.TotalTokens,
		DurationMS:       resp.DurationMS,
		TokensPerSecond:  tps,
	}
}

type offeredToolCatalog struct {
	Tools    []llm.Tool
	Policies map[string]governance.ToolPolicy
}

func (a *Agent) toolsForRequest(ctx context.Context, principal identity.Principal, exposure *toolruntime.Exposure, governor *governance.Governor) offeredToolCatalog {
	catalog := offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy)}
	add := func(tool llm.Tool, policy governance.ToolPolicy) {
		name := tool.Function.Name
		if _, exists := catalog.Policies[name]; name == "" || exists {
			return
		}
		if governor != nil && governor.IsToolRetired(name, policy) {
			return
		}
		catalog.Tools = append(catalog.Tools, tool)
		catalog.Policies[name] = policy
	}
	for _, tool := range a.registry.LLMToolsForVisibility(exposure.Visibility()) {
		if policy, ok := a.registry.Policy(tool.Function.Name); ok {
			add(tool, policy)
		}
	}
	if a.mcpProvider == nil {
		return catalog
	}
	for _, tool := range a.mcpProvider.DiscoveryTools(ctx, principal) {
		add(tool, a.mcpProvider.ToolPolicy(tool.Function.Name))
	}
	for _, tool := range a.mcpProvider.LLMTools(ctx, principal, exposure.ExposedMCPTools()) {
		add(tool, a.mcpProvider.ToolPolicy(tool.Function.Name))
	}
	return catalog
}

func (a *Agent) executeTool(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposure *toolruntime.Exposure) (governance.Result, error) {
	if a.registry.HasHandler(name) {
		return a.registry.Execute(ctx, name, args)
	}
	if a.mcpProvider != nil {
		if result, handled, err := a.mcpProvider.Execute(ctx, principal, name, args, exposure.ExposedMCPTools()); handled {
			return result.Result, err
		}
	}
	return a.registry.Execute(ctx, name, args)
}

func normalizeToolCallIDs(message *llm.ChatMessage, iteration int) {
	if message == nil {
		return
	}
	reserved := make(map[string]bool, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if id := strings.TrimSpace(call.ID); id != "" {
			reserved[id] = true
		}
	}
	used := make(map[string]bool, len(message.ToolCalls))
	for i := range message.ToolCalls {
		id := strings.TrimSpace(message.ToolCalls[i].ID)
		if id != "" && !used[id] {
			message.ToolCalls[i].ID = id
			used[id] = true
			continue
		}
		base := fmt.Sprintf("call_%d_%d", iteration, i+1)
		id = base
		for suffix := 2; reserved[id] || used[id]; suffix++ {
			id = fmt.Sprintf("%s_%d", base, suffix)
		}
		message.ToolCalls[i].ID = id
		used[id] = true
	}
}

func persistedToolCall(tc llm.ToolCall, policy governance.HistoryPolicy, decision governance.Decision, result governance.Result, execErr error, toolContent string, executedAt time.Time) usermemory.ToolHistoryCall {
	call := usermemory.ToolHistoryCall{
		Name:         strings.TrimSpace(tc.Function.Name),
		HistoryMode:  string(policy.Mode),
		Status:       "succeeded",
		Outcome:      string(result.Outcome),
		ReasonCode:   result.ReasonCode,
		IsDegraded:   result.IsDegraded,
		Result:       toolContent,
		ExecutedAt:   executedAt.Format(time.RFC3339Nano),
		SearchResult: policy.SearchResult,
	}
	if !decision.Allowed {
		call.Status = "blocked"
		call.Outcome = ""
		call.ReasonCode = decision.ReasonCode
	} else if execErr != nil {
		call.Status = "failed"
		call.Outcome = ""
		call.ReasonCode = "execution_error"
	}
	if policy.Mode == governance.HistoryMetadata {
		call.Arguments = map[string]interface{}{}
		call.Result = "Historical tool result omitted by policy."
		call.ArgumentsTruncated = true
		call.ResultTruncated = true
		call.SearchResult = false
		return call
	}
	call.Arguments = tc.Function.Arguments
	if call.Arguments == nil {
		call.Arguments = map[string]interface{}{}
	}
	if encoded, err := json.Marshal(call.Arguments); err != nil || len(encoded) > policy.MaxArgumentBytes {
		call.Arguments = map[string]interface{}{}
		call.ArgumentsTruncated = true
	}
	runes := []rune(call.Result)
	if len(runes) > policy.MaxResultRunes {
		notice := []rune("\n[Historical result truncated.]")
		keep := policy.MaxResultRunes - len(notice)
		if keep > 0 {
			call.Result = string(runes[:keep]) + string(notice)
		} else {
			call.Result = string(notice[:policy.MaxResultRunes])
		}
		call.ResultTruncated = true
	}
	return call
}

func governanceResultText(reason string) string {
	switch reason {
	case governance.ReasonDuplicate:
		return "Tool call blocked: the same tool and arguments were already executed in this request. Use the existing result or try meaningfully different arguments."
	case governance.ReasonToolLimit:
		return "Tool call blocked: this tool reached its execution limit for the request. Continue with the available results."
	case governance.ReasonToolFailures:
		return "Tool call blocked: the tool failure limit was reached. Continue without retrying this tool."
	case governance.ReasonToolUnproductive:
		return "Tool call blocked: this tool returned too many unproductive results. Continue with the available information."
	case governance.ReasonGlobalLimit, governance.ReasonIterationLimit:
		return "Tool call blocked: the request tool budget was exhausted. Finish the answer using the available results."
	case governance.ReasonUnadvertised:
		return "Tool call blocked: this tool was not available for this model step. Use only currently available tools."
	default:
		return "Tool call blocked by request policy. Continue with the available information."
	}
}

func (a *Agent) chatWithImageRetries(ctx context.Context, req llm.ChatRequest, callback func(llm.ChatMessage), log *config.Logger) (*llm.ChatResponse, error, bool) {
	originalMessages := req.Messages
	imageCount := 0
	for _, message := range originalMessages {
		imageCount += len(message.Images)
	}

	var firstErr error
	for attempt := 1; attempt <= maxImageModelAttempts; attempt++ {
		if imageCount > 0 {
			messages := append([]llm.ChatMessage(nil), originalMessages...)
			for i := range messages {
				if len(originalMessages[i].Images) == 0 {
					continue
				}
				resized, err := media.ResizeInputImagesForAttempt(originalMessages[i].Images, attempt, imageRetryScale, imageInitialScaleMaxEdge)
				if err != nil {
					log.Warn("agent.model.image_retry_resize_failed", "failed to resize images for model retry",
						config.F("attempt", attempt), config.F("image_count", imageCount),
						config.F("status", "degraded"), config.ErrorField(err))
					return nil, err, false
				}
				messages[i].Images = resized
			}
			req.Messages = messages
		}

		resp, err := a.chatClient.Chat(ctx, req, callback)
		if err == nil {
			return resp, nil, false
		}
		if imageCount == 0 || !llm.IsOllamaModelRunnerStoppedError(err) {
			return nil, err, false
		}
		if firstErr == nil {
			firstErr = err
		}
		if attempt == maxImageModelAttempts {
			log.Error("agent.model.image_retry_exhausted", "model runner stopped after resized image retries",
				config.F("attempt_count", attempt), config.F("image_count", imageCount),
				config.F("status", "error"), config.F("original_error", config.SafeErrorText(firstErr)),
				config.F("last_error", config.SafeErrorText(err)))
			return nil, err, true
		}
		log.Warn("agent.model.image_retry", "retrying model call with smaller images",
			config.F("attempt", attempt+1), config.F("image_count", imageCount),
			config.F("scale_percent", int(math.Pow(imageRetryScale, float64(attempt+1))*100)),
			config.F("status", "retry"))
	}
	return nil, firstErr, false
}

// Process handles the end-to-end agentic pipeline in a single loop.
// The model receives all registered tools and may call them zero or more times
// before generating its final response. Thinking tokens, content tokens, and
// agent status messages are streamed via streamCallback if provided.
//
// Tool execution errors are handled gracefully — failures inject an error tool
// response so the model can decide how to proceed. Provider errors are captured
// into AgentResponse.Error rather than returned as Go errors.
func (a *Agent) Process(ctx context.Context, request Request) (*AgentResponse, error) {
	if !request.Principal.Authenticated() {
		return nil, fmt.Errorf("agent request has no authenticated principal")
	}
	requestID := request.RequestID
	gateway := request.Principal.Gateway
	sessionKey := request.SessionKey
	senderID := request.Principal.CanonicalUserID
	displayName := request.DisplayName
	userPrompt := request.Prompt
	userImages := request.Images
	streamCallback := request.StreamFunc
	startedAt := time.Now()
	reqLog := a.log.Agent("agent", requestID, sessionKey, senderID, gateway, a.model)
	reqLog.Debug("agent.request.start", "agent request started",
		config.F("prompt_chars", len(userPrompt)),
		config.F("image_count", len(userImages)),
	)

	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Inject the resolved actor so tool handlers derive ownership from the same
	// principal used by gateways, commands, and the broker.
	ctx = requestctx.WithPrincipal(ctx, request.Principal)
	memoryStage := requestctx.NewMemoryStageCollector()
	ctx = requestctx.WithMemoryStageCollector(ctx, memoryStage)
	formationSourceText, _ := stripReplyContext(userPrompt)
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{
		RequestID:       requestID,
		SessionID:       sessionKey,
		Model:           a.model,
		CurrentUserText: formationSourceText,
	})
	contextImages := make([]requestctx.InputImage, 0, len(userImages))
	for _, image := range userImages {
		contextImages = append(contextImages, requestctx.InputImage{MIMEType: image.MimeType, Data: image.Data, Source: image.Source})
	}
	ctx = requestctx.WithInputImages(ctx, contextImages)
	toolExposure := toolruntime.NewExposure()
	if strings.EqualFold(strings.TrimSpace(gateway), "homeassistant") {
		toolExposure.HideBuiltins(toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage)
	} else if len(userImages) == 0 {
		toolExposure.HideBuiltins(toolnames.ComfyUIImageToImage)
	}
	ctx = requestctx.WithToolExposer(ctx, toolExposure)
	toolGovernor := governance.New(a.toolPolicy)

	// Read the operator-managed soul file fresh on every request.
	soulContent, soulErr := a.soul.Read()
	if soulErr != nil {
		reqLog.Warn("agent.soul.read_failed", "failed to read soul file", config.ErrorField(soulErr))
	}

	// Keep deployment policy separate from the frozen lower-authority tenant profile.
	var promptParts []string
	promptParts = append(promptParts, soulContent)
	if gatewayPrompt := gatewaySystemPrompt(gateway); gatewayPrompt != "" {
		promptParts = append(promptParts, gatewayPrompt)
	}
	dynamicSystemPrompt := strings.Join(promptParts, "\n\n")
	speakerLine := ""
	profileContent := ""
	sessionGeneration := 0
	if a.userMemory != nil {
		profile, err := a.userMemory.ResolveSessionProfile(ctx, senderID, sessionKey, sessionTurnTTL)
		if err != nil {
			reqLog.Error("agent.profile.load_failed", "failed to load tenant profile", config.F("status", "error"), config.ErrorField(err))
			return nil, fmt.Errorf("resolve tenant profile: %w", err)
		} else {
			speakerLine = profile.SpeakerIntro
			sessionGeneration = profile.Generation
			profileContent = profile.Content
			reqLog.Debug("agent.profile.loaded", "loaded frozen tenant profile",
				config.F("profile_version", profile.Version),
				config.F("latest_profile_version", profile.LatestVersion),
				config.F("profile_fact_count", profile.FactCount),
				config.F("profile_bytes", profile.Bytes),
				config.F("session_generation", profile.Generation),
				config.F("is_profile_new", profile.IsNewVersion),
				config.F("is_session_new", profile.IsNewSession),
			)
			if profile.IsNewVersion {
				reqLog.Info("agent.profile.version_advanced", "advanced tenant profile version",
					config.F("profile_version", profile.LatestVersion),
					config.F("profile_fact_count", profile.LatestFactCount),
					config.F("profile_bytes", profile.LatestBytes),
					config.F("status", "ok"),
				)
			}
			if profile.IsNewSession {
				reqLog.Info("agent.profile.session_bound", "bound tenant profile to session",
					config.F("profile_version", profile.Version),
					config.F("session_generation", profile.Generation),
					config.F("status", "ok"),
				)
			}
		}
	}
	requestUser := providerUserValue(firstNonEmpty(speakerLine, displayName, senderID))
	meta := requestctx.MetadataFromContext(ctx)
	meta.SessionGeneration = sessionGeneration
	ctx = requestctx.WithMetadata(ctx, meta)
	var recalledMemories []usermemory.RecallResult
	if a.userMemory != nil {
		recallQuery, _ := stripReplyContext(userPrompt)
		recallStarted := time.Now()
		var recallStats usermemory.RecallStats
		recalledMemories, recallStats = a.userMemory.Recall(ctx, senderID, recallQuery, usermemory.RecallRequest{TopK: automaticRecallTopK})
		if recallStats.LexicalError != nil {
			reqLog.Warn("agent.user_memory.recall.lexical_degraded", "user-memory lexical recall degraded", config.F("status", "degraded"), config.ErrorField(recallStats.LexicalError))
		}
		if recallStats.SemanticError != nil {
			reqLog.Warn("agent.user_memory.recall.semantic_degraded", "user-memory semantic recall degraded", config.F("status", "degraded"), config.ErrorField(recallStats.SemanticError))
		}
		reqLog.Debug("agent.user_memory.recall.complete", "completed user-memory recall",
			config.F("lexical_candidate_count", recallStats.LexicalCandidateCount),
			config.F("semantic_candidate_count", recallStats.SemanticCandidateCount),
			config.F("merged_candidate_count", recallStats.MergedCandidateCount),
			config.F("below_threshold_count", recallStats.BelowThresholdCount),
			config.F("selected_memory_count", recallStats.SelectedCount),
			config.F("min_selected_score", recallStats.MinSelectedScore),
			config.F("max_selected_score", recallStats.MaxSelectedScore),
			config.F("is_lexical_available", recallStats.LexicalAvailable),
			config.F("is_vector_available", recallStats.SemanticAvailable),
			config.F("duration_ms", time.Since(recallStarted).Milliseconds()),
		)
	}

	var recentTurns []usermemory.SessionTurn
	var recentToolNames []string
	var sessionSummary usermemory.SessionSummary
	if a.userMemory != nil && sessionGeneration > 0 {
		var err error
		sessionSummary, err = a.userMemory.LatestSessionSummary(ctx, senderID, sessionKey, sessionGeneration)
		if err != nil && err != sql.ErrNoRows {
			reqLog.Warn("agent.session_summary.load_failed", "failed to load session summary", config.F("status", "degraded"), config.ErrorField(err))
			sessionSummary = usermemory.SessionSummary{}
		}
		recentTools, toolErr := a.userMemory.RecentCompletedExchangesAfter(ctx, senderID, sessionKey, sessionGeneration, 0, recentToolExposureTurns)
		if toolErr != nil {
			reqLog.Warn("agent.session_memory.tools.failed", "failed to load recent tool continuity", config.F("status", "degraded"), config.ErrorField(toolErr))
		} else {
			for _, turn := range recentTools {
				recentToolNames = append(recentToolNames, turn.ToolNames...)
			}
			recentToolNames = uniqueToolNames(recentToolNames)
		}
		recentTurns, err = a.userMemory.RecentCompletedExchangesAfter(ctx, senderID, sessionKey, sessionGeneration, sessionSummary.CoveredThroughTurnID, sessionHistoryCandidateLimit)
		if err != nil {
			reqLog.Warn("agent.session_memory.context.failed", "failed to build session-memory context", config.F("status", "degraded"), config.ErrorField(err))
			recentTurns = nil
		} else {
			reqLog.Debug("agent.session_memory.context.loaded", "loaded session-memory context",
				config.F("candidate_turn_count", len(recentTurns)),
			)
		}
	}
	if a.mcpProvider != nil && len(recentToolNames) > 0 {
		mcpCandidates := make([]string, 0, len(recentToolNames))
		for _, name := range recentToolNames {
			if !a.registry.HasHandler(name) {
				mcpCandidates = append(mcpCandidates, name)
			}
		}
		toolExposure.ExposeTools(a.mcpProvider.ResolveTools(ctx, request.Principal, mcpCandidates))
	}
	var foregroundDebt []usermemory.SessionTurn
	if a.compactor != nil && a.userMemory != nil && sessionGeneration > 0 {
		boundary := sessionSummary.CoveredThroughTurnID
		var debtErr error
		foregroundDebt, debtErr = loadForegroundDeliveredDebt(ctx, a.userMemory, senderID, sessionKey, sessionGeneration, boundary)
		if debtErr != nil {
			return nil, fmt.Errorf("load foreground compaction debt: %w", debtErr)
		}
	}

	initialCatalog := a.toolsForRequest(ctx, request.Principal, toolExposure, toolGovernor)
	inputLimit := a.budget.UsableInputLimit()
	minimumTail := preservedRecentTailCount(recentTurns, inputLimit)
	promptContext := AssemblePromptContextWithSummary(dynamicSystemPrompt, profileContent, userPrompt, userImages, sessionSummary, minimumTail, recalledMemories, automaticRecallCharLimit, recentTurns, initialCatalog.Tools, inputLimit)
	if a.userMemory != nil {
		a.userMemory.RecordRecallUsage(ctx, senderID, promptContext.SelectedRecall)
	}
	messages := promptContext.Messages
	var previousSummary *usermemory.SessionSummary
	if sessionSummary.ID > 0 {
		previousSummary = &sessionSummary
	}
	foregroundCompaction := newForegroundCompactionState(a.compactor, inputLimit, dynamicSystemPrompt, profileContent, userPrompt, userImages, previousSummary, foregroundDebt, streamCallback)
	if promptContext.RequiredOverBudget {
		reqLog.Warn("agent.context.over_budget", "prompt still exceeds budget after compaction",
			config.F("estimated_after", promptContext.EstimatedAfter),
			config.F("prompt_budget", promptContext.InputLimit),
		)
	}
	reqLog.Debug("agent.context.selected", "selected complete session exchanges",
		config.F("selected_turn_count", promptContext.SelectedTurnCount),
		config.F("omitted_turn_count", promptContext.OmittedTurnCount),
		config.F("selected_memory_count", promptContext.SelectedRecallCount),
		config.F("omitted_memory_count", promptContext.OmittedRecallCount),
		config.F("recall_chars", promptContext.RecallChars),
		config.F("is_summary_included", promptContext.SummaryIncluded),
		config.F("summary_chars", promptContext.SummaryChars),
		config.F("minimum_tail_count", promptContext.MinimumTailCount),
		config.F("estimated_before", promptContext.EstimatedBefore),
		config.F("estimated_after", promptContext.EstimatedAfter),
	)

	req := llm.ChatRequest{
		Model:  a.model,
		User:   requestUser,
		Stream: streamCallback != nil,
	}

	// Track accumulated thinking and content across all iterations.
	// The model may emit thinking tokens in any iteration; content tokens only
	// appear in the final response turn (when no tool calls are made).
	var accumulatedThinking strings.Builder
	var accumulatedContent strings.Builder
	toolExecutionCount := 0

	// toolAnnotations collects brief notes about tools used this request.
	// These are appended to the stored assistant message so future turns
	// show what tools were called without ballooning history size.
	var toolAnnotations []string
	toolHistory := usermemory.EmptyToolHistory()

	// Build the streaming callback that routes thinking vs content chunks.
	// Tool-call iterations are streamed too — the model may reason aloud before
	// deciding to call a tool. The stream pauses naturally while tools execute.
	var chatCallback func(llm.ChatMessage)
	if streamCallback != nil {
		chatCallback = func(chunk llm.ChatMessage) {
			if chunk.Thinking != "" {
				accumulatedThinking.WriteString(chunk.Thinking)
				streamCallback(StreamChunk{Type: ChunkThinking, Text: chunk.Thinking})
			}
			if chunk.Content != "" {
				accumulatedContent.WriteString(chunk.Content)
				streamCallback(StreamChunk{Type: ChunkContent, Text: chunk.Content})
			}
		}
	}

	var lastResp *llm.ChatResponse
	var outputAttachments []media.OutputAttachment
	toolFailureBudgetExhausted := false
	toolGovernanceStopReason := ""
	temporaryParserFallback := false
	imageSizeFallbackUsed := false

	// Agentic loop: the model runs, may call tools, receives results, then runs again.
	// The loop exits when the model stops issuing tool calls, the request context
	// expires, or request-local tool governance exhausts a safety budget.
	for iteration := 1; ; iteration++ {
		// Reset the content accumulator each iteration — we only keep the final
		// response turn's content. Thinking is accumulated across all iterations.
		accumulatedContent.Reset()

		catalog := a.toolsForRequest(ctx, request.Principal, toolExposure, toolGovernor)
		req.Tools = catalog.Tools
		req.ToolChoice = ""
		preparedMessages, compactionStats, compactErr := foregroundCompaction.prepare(ctx, messages, req.Tools, false)
		if compactErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			reqLog.Warn("agent.context.compaction_failed", "failed to compact active request context",
				config.F("iteration", iteration), config.F("status", "degraded"), config.ErrorField(compactErr))
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkContent, Text: contextCompactionFallback})
			}
			return &AgentResponse{Model: a.model, Response: contextCompactionFallback, Attachments: outputAttachments}, nil
		}
		messages = preparedMessages
		req.Messages = messages
		if compactionStats.Compacted {
			reqLog.Info("agent.context.compacted", "compacted active request context",
				config.F("iteration", iteration), config.F("compacted_unit_count", compactionStats.DebtCount),
				config.F("estimated_before", compactionStats.EstimatedBefore), config.F("estimated_after", compactionStats.EstimatedAfter),
				config.F("prompt_budget", inputLimit), config.F("status", "ok"))
		}
		reqLog.Debug("agent.model.call", "calling model",
			config.F("iteration", iteration),
			config.F("is_streaming", req.Stream),
			config.F("tool_count", len(req.Tools)),
		)

		resp, err, imageRetriesExhausted := a.chatWithImageRetries(ctx, req, chatCallback, reqLog)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if llm.IsContextLengthExceededError(err) && foregroundCompaction.hasDebt() {
				var recoveryStats foregroundCompactionStats
				messages, recoveryStats, compactErr = foregroundCompaction.prepare(ctx, messages, req.Tools, true)
				if compactErr != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return nil, ctxErr
					}
				}
				if compactErr == nil && recoveryStats.Compacted {
					req.Messages = messages
					reqLog.Warn("agent.context.provider_overflow_recovery", "retrying model call after provider context overflow",
						config.F("iteration", iteration), config.F("compacted_unit_count", recoveryStats.DebtCount), config.F("status", "retry"))
					resp, err, imageRetriesExhausted = a.chatWithImageRetries(ctx, req, chatCallback, reqLog)
				}
			}
			if err != nil {
				reqLog.Error("agent.model.error", "model call failed", config.F("iteration", iteration), config.ErrorField(err))
			}
			if err != nil && llm.IsContextLengthExceededError(err) {
				if streamCallback != nil {
					streamCallback(StreamChunk{Type: ChunkContent, Text: contextCompactionFallback})
				}
				return &AgentResponse{Model: a.model, Response: contextCompactionFallback, Attachments: outputAttachments}, nil
			}
			if err == nil {
				// Continue with the response recovered after compaction.
			} else if imageRetriesExhausted {
				imageSizeFallbackUsed = true
				resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: imageSizeFallback}}
				if streamCallback != nil {
					streamCallback(StreamChunk{Type: ChunkContent, Text: imageSizeFallback})
				}
			} else if llm.IsTemporaryOllamaToolParserError(err) {
				// Temporary workaround for an upstream Ollama/Qwen tool-markup parser
				// defect. Retry the identical request once and remove this branch when fixed.
				reqLog.Warn("agent.model.temporary_parser_retry", "retrying model call after upstream tool parser failure",
					config.F("iteration", iteration),
					config.F("retry_attempt", 1),
					config.F("status", "retry"),
				)
				resp, err = a.chatClient.Chat(ctx, req, chatCallback)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				if err == nil {
					reqLog.Warn("agent.model.temporary_parser_retry_recovered", "model call recovered after upstream tool parser failure",
						config.F("iteration", iteration),
						config.F("retry_attempt", 1),
						config.F("is_recovered", true),
						config.F("status", "degraded"),
					)
				} else {
					reqLog.Error("agent.model.temporary_parser_retry_failed", "model retry failed after upstream tool parser failure",
						config.F("iteration", iteration),
						config.F("retry_attempt", 1),
						config.F("is_recovered", false),
						config.F("status", "error"),
						config.ErrorField(err),
					)
					if llm.IsTemporaryOllamaToolParserError(err) {
						temporaryParserFallback = true
						resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: emptyResponseFallback}}
						if streamCallback != nil {
							streamCallback(StreamChunk{Type: ChunkContent, Text: emptyResponseFallback})
						}
					} else {
						errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
						return &AgentResponse{Model: a.model, Response: errorText, Error: errorText}, nil
					}
				}
			} else {
				errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
				return &AgentResponse{Model: a.model, Response: errorText, Error: errorText}, nil
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		normalizeToolCallIDs(&resp.Message, iteration)
		lastResp = resp
		if iteration == 1 && resp.PromptTokens > 0 {
			reqLog.Debug("agent.context.estimated_vs_actual", "compared estimated and actual prompt tokens",
				config.F("estimated_after", promptContext.EstimatedAfter),
				config.F("actual_prompt_tokens", resp.PromptTokens),
			)
		}

		reqLog.Debug("agent.loop.iteration", "completed agent loop iteration",
			config.F("iteration", iteration),
			config.F("tool_call_count", len(resp.Message.ToolCalls)),
			config.F("thinking_chars", len(resp.Message.Thinking)),
			config.F("content_chars", len(resp.Message.Content)),
			config.F("failure_streak", toolGovernor.ConsecutiveFailures()),
		)

		// No tool calls — the model is done. Exit the loop.
		if len(resp.Message.ToolCalls) == 0 {
			reqLog.Debug("agent.loop.complete", "agent loop completed", config.F("iteration_count", iteration), config.F("status", "ok"))
			break
		}
		iterationDecision := toolGovernor.BeginToolIteration()

		// Append the assistant turn (including its tool calls) to the conversation.
		messages = append(messages, resp.Message)

		// Execute each tool call and inject the results as tool response messages.
		// NOTE: Most models only emit one tool call at a time, but we handle
		// multiple to be safe.
		historyBatch := usermemory.ToolHistoryBatch{AssistantContent: resp.Message.Content}
		foregroundBatch := usermemory.ToolHistoryBatch{AssistantContent: resp.Message.Content}
		for _, tc := range resp.Message.ToolCalls {
			toolName := tc.Function.Name
			if toolName == toolnames.UserMemorySave {
				historyBatch.AssistantContent = ""
			}
			toolCallID := tc.ID
			toolStartedAt := time.Now()

			// Emit a structured tool-call chunk so UIs can render the invocation.
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkToolCall, Tool: toolStreamPayload(toolName, tc.Function.Arguments, "", 0, false)})
			}
			reqLog.Info("agent.tool.start", "starting tool execution",
				config.F("iteration", iteration),
				config.F("tool_name", toolName),
			)

			var toolContent string
			var execErr error
			result := governance.Result{}
			policy, advertised := catalog.Policies[toolName]
			decision := governance.Decision{ReasonCode: iterationDecision.ReasonCode}
			if iterationDecision.Allowed {
				decision = toolGovernor.BeforeExecution(toolName, tc.Function.Arguments, policy, advertised)
			}
			if decision.Allowed {
				result, execErr = a.executeTool(ctx, request.Principal, toolName, tc.Function.Arguments, toolExposure)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				if execErr == nil && len(result.Attachments) > 0 {
					candidate := append(append([]media.OutputAttachment(nil), outputAttachments...), result.Attachments...)
					if result.Outcome != governance.OutcomeProductive {
						execErr = fmt.Errorf("tool returned attachments without a productive result")
					} else if attachmentErr := media.ValidateOutputAttachments(candidate); attachmentErr != nil {
						execErr = fmt.Errorf("tool returned invalid attachments: %w", attachmentErr)
					} else {
						outputAttachments = candidate
					}
				}
				toolGovernor.RecordResult(toolName, decision, result, execErr)
			} else {
				toolContent = governanceResultText(decision.ReasonCode)
				reqLog.Warn("agent.tool.blocked", "blocked tool execution",
					config.F("iteration", iteration), config.F("tool_name", toolName),
					config.F("reason_code", decision.ReasonCode), config.F("status", "rejected"))
			}
			if decision.Allowed && execErr != nil {
				// Fail gracefully: inject the error so the model can recover.
				reqLog.Warn("agent.tool.failure", "tool execution failed",
					config.F("iteration", iteration),
					config.F("tool_name", toolName),
					config.F("failure_streak", toolGovernor.ConsecutiveFailures()),
					config.F("max_failures", a.toolPolicy.MaxConsecutiveFailures),
					config.F("duration_ms", time.Since(toolStartedAt).Milliseconds()),
					config.F("status", "error"),
					config.ErrorField(execErr),
				)
				toolContent = "Error: " + truncate(config.SafeErrorText(execErr), 1000)
			} else if decision.Allowed {
				toolContent = result.Content
				status := "ok"
				if result.IsDegraded {
					status = "degraded"
				}
				reqLog.Debug("agent.tool.success", "tool execution succeeded",
					config.F("iteration", iteration),
					config.F("tool_name", toolName),
					config.F("tool_outcome", result.Outcome),
					config.F("reason_code", result.ReasonCode),
					config.F("is_degraded", result.IsDegraded),
					config.F("duration_ms", time.Since(toolStartedAt).Milliseconds()),
					config.F("status", status),
				)
				// Keep the successful-name projection for MCP continuity and legacy turns.
				toolAnnotations = append(toolAnnotations, toolName)
			}
			if decision.Allowed {
				toolExecutionCount++
			}
			if streamCallback != nil {
				var streamAttachments []media.OutputAttachment
				if decision.Allowed && execErr == nil && len(result.Attachments) > 0 {
					streamAttachments = append([]media.OutputAttachment(nil), result.Attachments...)
				}
				streamCallback(StreamChunk{
					Type:        ChunkToolResult,
					Tool:        toolStreamPayload(toolName, tc.Function.Arguments, toolContent, time.Since(toolStartedAt), execErr != nil || !decision.Allowed),
					Attachments: streamAttachments,
				})
			}

			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				ToolName:   toolName,
				ToolCallID: toolCallID,
				Content:    toolContent,
			})
			historyPolicy := policy.History.Effective()
			if !advertised {
				historyPolicy.Mode = governance.HistoryMetadata
				historyPolicy.SearchResult = false
			}
			if historyPolicy.Mode != governance.HistoryNone {
				historyBatch.Calls = append(historyBatch.Calls, persistedToolCall(tc, historyPolicy, decision, result, execErr, toolContent, time.Now().UTC()))
			}
			foregroundPolicy := historyPolicy
			if foregroundPolicy.Mode == governance.HistoryNone {
				foregroundPolicy.Mode = governance.HistoryMetadata
				foregroundPolicy.SearchResult = false
			}
			foregroundBatch.Calls = append(foregroundBatch.Calls, persistedToolCall(tc, foregroundPolicy, decision, result, execErr, toolContent, time.Now().UTC()))
			stats := toolGovernor.Stats(toolName)
			reqLog.Debug("agent.tool.governance", "updated request-local tool governance",
				config.F("tool_name", toolName),
				config.F("tool_attempt_count", stats.Attempts),
				config.F("tool_execution_count", stats.Executions),
				config.F("tool_productive_count", stats.Productive),
				config.F("tool_unproductive_count", stats.Unproductive),
				config.F("tool_failure_count", stats.Failures),
				config.F("tool_duplicate_count", stats.Duplicates),
				config.F("tool_blocked_count", stats.Blocked),
				config.F("is_tool_retired", advertised && toolGovernor.IsToolRetired(toolName, policy)))
		}
		if len(historyBatch.Calls) > 0 {
			toolHistory.Batches = append(toolHistory.Batches, historyBatch)
		}
		foregroundCompaction.addToolBatch(foregroundBatch, userPrompt)
		if reason := toolGovernor.GlobalStopReason(); reason != "" {
			toolGovernanceStopReason = reason
			toolFailureBudgetExhausted = reason == governance.ReasonToolFailures
			reqLog.Warn("agent.tool_budget.exhausted", "tool governance budget exhausted",
				config.F("reason_code", reason),
				config.F("failure_streak", toolGovernor.ConsecutiveFailures()),
				config.F("tool_execution_count", toolGovernor.TotalExecutions()),
				config.F("tool_iteration_count", toolGovernor.ToolIterations()),
				config.F("status", "degraded"))
			break
		}
	}

	if toolGovernanceStopReason != "" {
		accumulatedContent.Reset()
		finalReq := req
		finalReq.Tools = nil
		preparedMessages, compactionStats, compactErr := foregroundCompaction.prepare(ctx, messages, nil, false)
		if compactErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			reqLog.Warn("agent.context.compaction_failed", "failed to compact active request before final model call", config.F("status", "degraded"), config.ErrorField(compactErr))
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkContent, Text: contextCompactionFallback})
			}
			return &AgentResponse{Model: a.model, Response: contextCompactionFallback, Attachments: outputAttachments}, nil
		}
		messages = preparedMessages
		finalReq.Messages = messages
		if compactionStats.Compacted {
			reqLog.Info("agent.context.compacted", "compacted active request context before final model call",
				config.F("compacted_unit_count", compactionStats.DebtCount), config.F("estimated_before", compactionStats.EstimatedBefore),
				config.F("estimated_after", compactionStats.EstimatedAfter), config.F("prompt_budget", inputLimit), config.F("status", "ok"))
		}

		resp, err, imageRetriesExhausted := a.chatWithImageRetries(ctx, finalReq, chatCallback, reqLog)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if llm.IsContextLengthExceededError(err) && foregroundCompaction.hasDebt() {
				preparedMessages, recoveryStats, compactErr := foregroundCompaction.prepare(ctx, messages, nil, true)
				if compactErr != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return nil, ctxErr
					}
				} else if recoveryStats.Compacted {
					messages = preparedMessages
					finalReq.Messages = messages
					resp, err, imageRetriesExhausted = a.chatWithImageRetries(ctx, finalReq, chatCallback, reqLog)
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if err != nil {
				reqLog.Error("agent.model.error", "model finish failed after tool failures", config.ErrorField(err))
				if llm.IsContextLengthExceededError(err) {
					resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: contextCompactionFallback}}
					if streamCallback != nil {
						streamCallback(StreamChunk{Type: ChunkContent, Text: contextCompactionFallback})
					}
				} else if imageRetriesExhausted {
					imageSizeFallbackUsed = true
					resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: imageSizeFallback}}
					if streamCallback != nil {
						streamCallback(StreamChunk{Type: ChunkContent, Text: imageSizeFallback})
					}
				} else {
					errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
					return &AgentResponse{Model: a.model, Response: errorText, Error: errorText}, nil
				}
			}
		}

		lastResp = resp
		reqLog.Debug("agent.loop.complete", "completed agent loop after disabling tools",
			config.F("iteration_count", toolExecutionCount+1),
			config.F("failure_streak", toolGovernor.ConsecutiveFailures()),
			config.F("reason_code", toolGovernanceStopReason),
			config.F("status", "degraded"),
		)
	}

	// Extract the final response content. The LLM client already handles
	// thinking-to-content promotion for non-streaming calls.
	// For streaming, we tracked content separately via the callback above.
	finalContent := accumulatedContent.String()
	if finalContent == "" && lastResp != nil {
		finalContent = lastResp.Message.Content
	}

	finalThinking := accumulatedThinking.String()
	if finalThinking == "" && lastResp != nil {
		finalThinking = lastResp.Message.Thinking
	}
	if strings.TrimSpace(finalContent) == "" {
		retryMessages := append([]llm.ChatMessage{}, messages...)
		retryMessages = append(retryMessages, llm.ChatMessage{Role: "user", Content: emptyResponseRetryPrompt})
		preparedMessages, compactionStats, compactErr := foregroundCompaction.prepare(ctx, retryMessages, nil, false)
		if compactErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			reqLog.Warn("agent.context.compaction_failed", "failed to compact active request before empty-response retry", config.F("status", "degraded"), config.ErrorField(compactErr))
			finalContent = contextCompactionFallback
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkContent, Text: finalContent})
			}
		} else if compactionStats.Compacted {
			messages = preparedMessages
			retryMessages = append(append([]llm.ChatMessage{}, messages...), llm.ChatMessage{Role: "user", Content: emptyResponseRetryPrompt})
		}

		if finalContent == "" {
			accumulatedContent.Reset()
			retryReq := req
			retryReq.Messages = retryMessages
			retryReq.Tools = nil

			reqLog.Warn("agent.response.empty_retry", "model returned no visible response; retrying once",
				config.F("thinking_chars", len(finalThinking)),
				config.F("status", "retry"),
			)
			retryResp, err, imageRetriesExhausted := a.chatWithImageRetries(ctx, retryReq, chatCallback, reqLog)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				if llm.IsContextLengthExceededError(err) && foregroundCompaction.hasDebt() {
					preparedMessages, recoveryStats, compactErr := foregroundCompaction.prepare(ctx, retryMessages, nil, true)
					if compactErr != nil {
						if ctxErr := ctx.Err(); ctxErr != nil {
							return nil, ctxErr
						}
					} else if recoveryStats.Compacted {
						messages = preparedMessages
						retryMessages = append(append([]llm.ChatMessage{}, messages...), llm.ChatMessage{Role: "user", Content: emptyResponseRetryPrompt})
						retryReq.Messages = retryMessages
						retryResp, err, imageRetriesExhausted = a.chatWithImageRetries(ctx, retryReq, chatCallback, reqLog)
					}
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if err != nil {
				reqLog.Warn("agent.response.empty_retry_failed", "empty-response retry failed",
					config.F("status", "degraded"),
					config.ErrorField(err),
				)
				if llm.IsContextLengthExceededError(err) {
					finalContent = contextCompactionFallback
					if streamCallback != nil {
						streamCallback(StreamChunk{Type: ChunkContent, Text: finalContent})
					}
				} else if imageRetriesExhausted {
					imageSizeFallbackUsed = true
					finalContent = imageSizeFallback
					if streamCallback != nil {
						streamCallback(StreamChunk{Type: ChunkContent, Text: imageSizeFallback})
					}
				}
			} else {
				lastResp = retryResp
				finalContent = accumulatedContent.String()
				if strings.TrimSpace(finalContent) == "" {
					finalContent = retryResp.Message.Content
				}
				if retryResp.Message.Thinking != "" && !strings.Contains(finalThinking, retryResp.Message.Thinking) {
					finalThinking += retryResp.Message.Thinking
				}
			}
		}

		if strings.TrimSpace(finalContent) == "" {
			finalContent = emptyResponseFallback
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkContent, Text: finalContent})
			}
			reqLog.Warn("agent.response.empty_fallback", "using generic fallback after empty model response",
				config.F("status", "degraded"),
			)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if lastResp != nil {
		messages = append(messages, lastResp.Message)
	}
	userMemoryContent := sessionMemoryUserContent(userPrompt, len(userImages))
	stagedMemory := memoryStage.Candidates()
	if len(stagedMemory) > 0 && (a.userMemory == nil || sessionGeneration <= 0) {
		return nil, fmt.Errorf("persist staged foreground memory: session storage is unavailable")
	}
	var storedTurn usermemory.StoredSessionTurn
	if finalContent != "" && a.userMemory != nil && sessionGeneration > 0 {
		storedReplay := usermemory.SessionTurn{UserText: userMemoryContent, AssistantText: finalContent, ToolNames: uniqueToolNames(toolAnnotations), ToolHistory: toolHistory}
		completedPressure := completedPromptPressure(promptContext, storedReplay)
		var err error
		storedTurn, err = a.userMemory.AppendSessionTurnForGenerationResultWithPressureHistoryAndForegroundMemory(ctx, sessionKey, senderID, sessionGeneration, userMemoryContent, finalContent, toolAnnotations, toolHistory, stagedMemory, sessionTurnTTL, usermemory.SessionPromptPressure{Tokens: completedPressure, Limit: promptContext.InputLimit, Version: promptPressureVersion(a.model, promptContext.InputLimit)})
		if err != nil {
			reqLog.Warn("agent.session_memory.write_failed", "failed to append session memory after turn", config.F("status", "degraded"), config.ErrorField(err))
			if len(stagedMemory) > 0 {
				return nil, fmt.Errorf("persist staged foreground memory: %w", err)
			}
		} else if len(stagedMemory) > 0 && storedTurn.ID == 0 {
			return nil, fmt.Errorf("persist staged foreground memory: session turn was not stored")
		}
	}

	responseStatus := "ok"
	if temporaryParserFallback || imageSizeFallbackUsed || toolGovernanceStopReason != "" {
		responseStatus = "degraded"
	}
	reqLog.Info("agent.response.complete", "completed agent response",
		config.F("iteration_count", toolExecutionCount+1),
		config.F("response_chars", len(finalContent)),
		config.F("thinking_chars", len(finalThinking)),
		config.F("tool_call_count", toolExecutionCount),
		config.F("duration_ms", time.Since(startedAt).Milliseconds()),
		config.F("is_tool_failure_budget_exhausted", toolFailureBudgetExhausted),
		config.F("status", responseStatus),
	)

	return &AgentResponse{
		Model:             a.model,
		Response:          finalContent,
		Thinking:          finalThinking,
		Metrics:           mapMetrics(lastResp),
		Attachments:       outputAttachments,
		SourceTurnID:      storedTurn.ID,
		SessionGeneration: storedTurn.Generation,
	}, nil
}

func promptPressureVersion(model string, inputLimit int) string {
	return fmt.Sprintf("%s:%s:%d", sessionPromptPressurePrefix, strings.TrimSpace(model), inputLimit)
}
