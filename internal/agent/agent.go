package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/exposure"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

const (
	sessionHistoryCandidateLimit = 1000
	recentToolExposureTurns      = 4
	automaticRecallTopK          = 4
	automaticRecallCharLimit     = 2000
	sessionTurnTTL               = 24 * time.Hour
	emptyResponseRetryPrompt     = "Your previous completion contained no visible response. Answer the user's last request now using only visible response content."
	emptyResponseFallback        = "I blanked on the actual answer. Try again and I'll take another shot."
	contextCompactionFallback    = "I cannot continue because I ran out of context. Any completed actions have not been undone. Try splitting the remaining task into smaller steps."
	sessionPromptPressurePrefix  = "session-prompt-pressure-v1"
)

// Agent handles LLM orchestration: a single agentic loop where the model
// calls tools from the registry and generates the final response.
type Agent struct {
	chatClient  llm.Chatter
	registry    *registry.Registry
	mcpProvider MCPProvider
	budget      tokenbudget.ContextBudget
	model       string
	soul        *soul.Store
	userMemory  *memory.Store
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

// NewAgent initializes the Agent with an LLM chat client, tool registry, model name,
// soul store, SQLite user memory store, prompt budget, tool-governance policy,
// and logger.
func NewAgent(
	chatClient llm.Chatter,
	registry *registry.Registry,
	model string,
	soul *soul.Store,
	userMemory *memory.Store,
	budget tokenbudget.ContextBudget,
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

// Process handles the end-to-end agentic pipeline in a single loop.
// The model receives all registered tools and may call them zero or more times
// before generating its final response. Thinking tokens, content tokens, and
// agent status messages are streamed via streamCallback if provided.
//
// Tool execution errors are handled gracefully — failures inject an error tool
// response so the model can decide how to proceed. Provider errors are captured
// into Response.Error rather than returned as Go errors.
func (a *Agent) Process(ctx context.Context, request Request) (response *Response, processErr error) {
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
	reqLog := a.log.Agent("agent", requestID, senderID, gateway, a.model)
	modelIterations, toolExecutionCount, toolBlockedCount := 0, 0, 0
	persistenceStatus := "not_attempted"
	usage := requestctx.UsageCollectorFromContext(ctx)
	usage.SetExecution(requestctx.ExecutionSnapshot{Model: a.model, PersistenceStatus: "unknown"})
	defer func() {
		kind, status := "answer", "ok"
		if processErr != nil {
			kind, status = "error", "error"
		}
		if errors.Is(processErr, context.Canceled) {
			kind, status = "canceled", "ok"
		}
		if response != nil {
			response.ToolExecutionCount, response.ToolBlockedCount = toolExecutionCount, toolBlockedCount
			if response.Kind == "" {
				response.Kind = "answer"
			}
			if response.Error != "" {
				response.Kind = "provider_error"
			}
			kind = response.Kind
			response.PersistenceStatus = persistenceStatus
			if kind != "answer" {
				status = "degraded"
			}
			if kind == "provider_error" {
				status = "error"
			}
		}
		usage.SetExecution(requestctx.ExecutionSnapshot{ToolExecutionCount: toolExecutionCount, BlockedCount: toolBlockedCount,
			PersistenceStatus: persistenceStatus, ResponseKind: kind, Model: a.model, IsComplete: true})
		reqLog.Info("agent.response.complete", "completed agent generation", config.F("iteration_count", modelIterations),
			config.F("record_kind", "summary"), config.F("is_execution_complete", true), config.F("tool_blocked_count", toolBlockedCount),
			config.F("tool_execution_count", toolExecutionCount), config.F("duration_ms", time.Since(startedAt).Milliseconds()),
			config.F("response_kind", kind), config.F("persistence_status", persistenceStatus), config.F("status", status))
	}()
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
	inherited := requestctx.MetadataFromContext(ctx)
	inherited.RequestID, inherited.SessionID, inherited.Model, inherited.CurrentUserText = requestID, sessionKey, a.model, formationSourceText
	if inherited.Workload == "" {
		inherited.Workload = "foreground"
	}
	ctx = requestctx.WithMetadata(ctx, inherited)
	reqLog = reqLog.With(requestctx.LogFields(ctx)...)
	contextImages := make([]requestctx.InputImage, 0, len(userImages))
	for _, image := range userImages {
		contextImages = append(contextImages, requestctx.InputImage{MIMEType: image.MimeType, Data: image.Data, Source: image.Source})
	}
	ctx = requestctx.WithInputImages(ctx, contextImages)
	toolExposure := exposure.NewExposure()
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
	var recalledMemories []memory.RecallResult
	if a.userMemory != nil {
		recallQuery, _ := stripReplyContext(userPrompt)
		recallStarted := time.Now()
		var recallStats memory.RecallStats
		recalledMemories, recallStats = a.userMemory.Recall(ctx, senderID, recallQuery, memory.RecallRequest{TopK: automaticRecallTopK})
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

	var recentTurns []memory.SessionTurn
	var recentToolNames []string
	var sessionSummary memory.SessionSummary
	if a.userMemory != nil && sessionGeneration > 0 {
		var err error
		sessionSummary, err = a.userMemory.LatestSessionSummary(ctx, senderID, sessionKey, sessionGeneration)
		if err != nil && err != sql.ErrNoRows {
			reqLog.Warn("agent.session_summary.load_failed", "failed to load session summary", config.F("status", "degraded"), config.ErrorField(err))
			sessionSummary = memory.SessionSummary{}
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
	var foregroundDebt []memory.SessionTurn
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
	promptContext := AssemblePromptContext(dynamicSystemPrompt, profileContent, userPrompt, userImages, sessionSummary, minimumTail, recalledMemories, automaticRecallCharLimit, recentTurns, initialCatalog.Tools, inputLimit)
	if a.userMemory != nil {
		a.userMemory.RecordRecallUsage(ctx, senderID, promptContext.SelectedRecall)
	}
	messages := promptContext.Messages
	var previousSummary *memory.SessionSummary
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

	// toolAnnotations collects brief notes about tools used this request.
	// These are appended to the stored assistant message so future turns
	// show what tools were called without ballooning history size.
	var toolAnnotations []string
	toolHistory := memory.EmptyToolHistory()

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
	toolGovernanceStopReason := ""
	temporaryParserFallback := false
	imageSizeFallbackUsed := false
	useContextFallback := func() {
		accumulatedContent.Reset()
		accumulatedContent.WriteString(contextCompactionFallback)
		lastResp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: contextCompactionFallback}}
		if streamCallback != nil {
			streamCallback(StreamChunk{Type: ChunkContent, Text: contextCompactionFallback})
		}
	}

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
		initialPressure := iteration == 1 && promptContext.EstimatedBefore*100 >= inputLimit*tokenbudget.CompactionTriggerPercent
		preparedMessages, compactionStats, compactErr := foregroundCompaction.prepare(ctx, messages, req.Tools, initialPressure)
		if compactErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			reqLog.Warn("agent.context.compaction_failed", "failed to compact active request context",
				config.F("iteration", iteration), config.F("status", "degraded"), config.ErrorField(compactErr))
			useContextFallback()
			goto finalize
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

		modelIterations++
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
					modelIterations++
					resp, err, imageRetriesExhausted = a.chatWithImageRetries(ctx, req, chatCallback, reqLog)
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if err != nil && llm.IsContextLengthExceededError(err) {
				useContextFallback()
				goto finalize
			}
			if err == nil {
				// Continue with the response recovered after compaction.
				reqLog.Info("agent.model.context_retry_recovered", "model recovered after context compaction", config.F("status", "ok"))
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
				modelIterations++
				resp, err = a.chatClient.Chat(ctx, req, chatCallback)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				if err == nil {
					reqLog.Info("agent.model.temporary_parser_retry_recovered", "model call recovered after upstream tool parser failure",
						config.F("iteration", iteration),
						config.F("retry_attempt", 1),
						config.F("is_recovered", true),
						config.F("status", "degraded"),
					)
				} else {
					reqLog.Debug("agent.model.temporary_parser_retry_failed", "model retry failed after upstream tool parser failure",
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
						return &Response{Model: a.model, Response: errorText, Error: errorText}, nil
					}
				}
			} else {
				errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
				return &Response{Model: a.model, Response: errorText, Error: errorText}, nil
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
		historyBatch := memory.ToolHistoryBatch{AssistantContent: resp.Message.Content}
		foregroundBatch := memory.ToolHistoryBatch{AssistantContent: resp.Message.Content}
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

			var toolContent string
			var execErr error
			result := governance.Result{}
			policy, advertised := catalog.Policies[toolName]
			decision := governance.Decision{ReasonCode: iterationDecision.ReasonCode}
			if iterationDecision.Allowed {
				decision = toolGovernor.BeforeExecution(toolName, tc.Function.Arguments, policy, advertised)
			}
			if decision.Allowed {
				toolExecutionCount++
				usage.SetExecution(requestctx.ExecutionSnapshot{ToolExecutionCount: toolExecutionCount, BlockedCount: toolBlockedCount, PersistenceStatus: "unknown", Model: a.model})
				toolMeta := requestctx.MetadataFromContext(ctx)
				toolMeta.ParentOperationID = toolMeta.OperationID
				toolMeta.OperationID = config.NewRequestID()
				reqLog.Debug("agent.tool.start", "starting authorized tool execution", config.F("tool_name", toolName))
				result, execErr = a.executeTool(requestctx.WithMetadata(ctx, toolMeta), request.Principal, toolName, tc.Function.Arguments, toolExposure)
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
				status, outcome := "ok", string(result.Outcome)
				if result.IsDegraded {
					status = "degraded"
				}
				if execErr != nil {
					status, outcome = "error", "error"
				}
				if errors.Is(execErr, context.Canceled) {
					status, outcome = "ok", "canceled"
				}
				scope := "mcp"
				if a.registry.HasHandler(toolName) {
					scope = "builtin"
				}
				reqLog.Info("agent.tool.complete", "completed tool execution", config.F("tool_name", toolName), config.F("scope", scope),
					config.F("record_kind", "measurement"), config.F("reason_code", result.ReasonCode), config.ErrorField(execErr),
					config.F("operation_id", toolMeta.OperationID), config.F("parent_operation_id", toolMeta.ParentOperationID),
					config.F("duration_ms", time.Since(toolStartedAt).Milliseconds()), config.F("outcome", outcome), config.F("status", status))
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
			} else {
				toolBlockedCount++
				usage.SetExecution(requestctx.ExecutionSnapshot{ToolExecutionCount: toolExecutionCount, BlockedCount: toolBlockedCount, PersistenceStatus: "unknown", Model: a.model})
				toolContent = governanceResultText(decision.ReasonCode)
				safeName := toolName
				if !advertised {
					safeName = "unadvertised"
				}
				reqLog.Info("agent.tool.blocked", "blocked tool execution",
					config.F("iteration", iteration), config.F("tool_name", safeName),
					config.F("reason_code", decision.ReasonCode), config.F("status", "rejected"))
			}
			if decision.Allowed && execErr != nil {
				// Fail gracefully: inject the error so the model can recover.
				reqLog.Warn("agent.tool.failure", "tool execution failed",
					config.F("iteration", iteration),
					config.F("tool_name", toolName),
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
			// Active checkpoints need exact model-visible evidence, not durable-history redaction.
			foregroundBatch.Calls = append(foregroundBatch.Calls, foregroundToolCall(tc, decision, result, execErr, toolContent, time.Now().UTC()))
			stats := toolGovernor.Stats(toolName)
			loggedToolName := toolName
			if !advertised {
				loggedToolName = "unadvertised"
			}
			reqLog.Debug("agent.tool.governance", "updated request-local tool governance",
				config.F("tool_name", loggedToolName),
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
			reqLog.Warn("agent.tool_budget.exhausted", "tool governance budget exhausted",
				config.F("reason_code", reason),
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
			useContextFallback()
			goto finalize
		}
		messages = preparedMessages
		finalReq.Messages = messages
		if compactionStats.Compacted {
			reqLog.Info("agent.context.compacted", "compacted active request context before final model call",
				config.F("compacted_unit_count", compactionStats.DebtCount), config.F("estimated_before", compactionStats.EstimatedBefore),
				config.F("estimated_after", compactionStats.EstimatedAfter), config.F("prompt_budget", inputLimit), config.F("status", "ok"))
		}

		modelIterations++
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
					modelIterations++
					resp, err, imageRetriesExhausted = a.chatWithImageRetries(ctx, finalReq, chatCallback, reqLog)
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if err != nil {
				if llm.IsContextLengthExceededError(err) {
					useContextFallback()
					goto finalize
				} else if imageRetriesExhausted {
					imageSizeFallbackUsed = true
					resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: imageSizeFallback}}
					if streamCallback != nil {
						streamCallback(StreamChunk{Type: ChunkContent, Text: imageSizeFallback})
					}
				} else {
					errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
					return &Response{Model: a.model, Response: errorText, Error: errorText}, nil
				}
			}
		}

		lastResp = resp
		reqLog.Debug("agent.loop.complete", "completed agent loop after disabling tools",
			config.F("iteration_count", modelIterations),
			config.F("reason_code", toolGovernanceStopReason),
			config.F("status", "degraded"),
		)
	}

finalize:
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
		finalContent = ""
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
			modelIterations++
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
						modelIterations++
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
				if strings.TrimSpace(finalContent) != "" {
					reqLog.Info("agent.response.empty_retry_recovered", "model recovered after empty response", config.F("status", "ok"))
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
		persistenceStatus = "failed"
		return nil, fmt.Errorf("persist staged foreground memory: session storage is unavailable")
	}
	var storedTurn memory.StoredSessionTurn
	if finalContent != "" && a.userMemory != nil && sessionGeneration > 0 {
		persistenceStatus = "failed"
		storedReplay := memory.SessionTurn{UserText: userMemoryContent, AssistantText: finalContent, ToolNames: uniqueToolNames(toolAnnotations), ToolHistory: toolHistory}
		completedPressure := tokenbudget.EstimateCompletedRequest(promptContext.EstimatedBefore, storedReplay.UserText, memory.SessionTurnMessages(storedReplay))
		var err error
		storedTurn, err = a.userMemory.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{SessionID: sessionKey, UserID: senderID, Generation: sessionGeneration, UserText: userMemoryContent, AssistantText: finalContent, ToolNames: toolAnnotations, History: toolHistory, Staged: stagedMemory, TTL: sessionTurnTTL, Pressure: memory.SessionPromptPressure{Tokens: completedPressure, Limit: promptContext.InputLimit, Version: promptPressureVersion(a.model, promptContext.InputLimit)}})
		if err != nil {
			reqLog.Warn("agent.session_memory.write_failed", "failed to append session memory after turn", config.F("status", "degraded"), config.ErrorField(err))
			if len(stagedMemory) > 0 {
				return nil, fmt.Errorf("persist staged foreground memory: %w", err)
			}
		} else if len(stagedMemory) > 0 && storedTurn.ID == 0 {
			return nil, fmt.Errorf("persist staged foreground memory: session turn was not stored")
		}
		if storedTurn.ID > 0 {
			persistenceStatus = "pending"
		}
	}

	responseStatus := "ok"
	if temporaryParserFallback || imageSizeFallbackUsed || toolGovernanceStopReason != "" || finalContent == contextCompactionFallback {
		responseStatus = "degraded"
	}
	reqLog.Debug("agent.response.detail", "completed agent response",
		config.F("iteration_count", modelIterations),
		config.F("response_chars", len(finalContent)),
		config.F("thinking_chars", len(finalThinking)),
		config.F("tool_call_count", toolExecutionCount),
		config.F("duration_ms", time.Since(startedAt).Milliseconds()),
		config.F("status", responseStatus),
	)

	responseKind := "answer"
	if toolGovernanceStopReason != "" {
		responseKind = "tool_limit"
	}
	if temporaryParserFallback {
		responseKind = "parser_fallback"
	}
	if imageSizeFallbackUsed {
		responseKind = "image_fallback"
	}
	if finalContent == contextCompactionFallback {
		responseKind = "context_fallback"
	}
	if finalContent == emptyResponseFallback && !temporaryParserFallback {
		responseKind = "empty_fallback"
	}
	return &Response{
		Kind:              responseKind,
		Model:             a.model,
		Response:          finalContent,
		Thinking:          finalThinking,
		Metrics:           mapMetrics(lastResp),
		Attachments:       outputAttachments,
		SourceTurnID:      storedTurn.ID,
		SessionGeneration: storedTurn.Generation,
	}, nil
}
