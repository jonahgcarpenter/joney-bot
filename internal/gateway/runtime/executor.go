package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/usermanagement"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// Execute applies shared routing policy, command handling, and broker submission.
func Execute(req Request, deps Dependencies, responder Responder) (outcome Outcome) {
	if req.RequestID == "" {
		req.RequestID = config.NewRequestID()
	}
	gateway := "unknown"
	switch req.Principal.Gateway {
	case "discord", "imessage", "homeassistant":
		gateway = req.Principal.Gateway
	}
	log := deps.Log.Server("gateway.runtime", config.F("gateway", gateway))
	if req.ReceivedAt.IsZero() {
		req.ReceivedAt = time.Now()
	}
	promptType := "empty"
	if strings.TrimSpace(req.Text) != "" {
		promptType = "text"
	}
	if len(req.Images) > 0 {
		promptType = "image"
		if strings.TrimSpace(req.Text) != "" {
			promptType = "text_image"
		}
	} else if promptType == "empty" && len(req.Unsupported) > 0 {
		promptType = "unsupported"
	}
	decision := routing.Decide(routing.Input{
		IsGroup:            req.IsGroup,
		IsMention:          req.IsMention,
		IsReplyToBot:       req.IsReplyToBot,
		IsCommandAttempt:   commands.IsAttempt(req.Text),
		Text:               req.Text,
		CurrentImages:      req.Images,
		CurrentUnsupported: req.Unsupported,
		Reply:              req.Reply,
	})
	if decision.Action == routing.ActionIgnore {
		return Outcome{Action: decision.Action, Reason: decision.Reason}
	}
	userID := ""
	kind, responseKind := "prompt", "answer"
	commandName := "unknown"
	if decision.Action == routing.ActionCommand {
		kind, responseKind = "command", "command"
		parsed, _ := commands.Parse(decision.Prompt)
		if deps.Commands != nil {
			commandName = deps.Commands.CanonicalName(parsed.Name)
		}
	} else if decision.Action == routing.ActionGatewayFallback {
		kind, responseKind = "fallback", "fallback"
	}
	executionStatus, persistenceStatus := "ok", "not_attempted"
	reasonCode := decision.Reason
	var terminalErr error
	var model, terminalErrorCode string
	var toolExecutionCount, toolBlockedCount int
	isAdmitted := false
	isExecutionComplete := true
	var queueWaitMS, agentDurationMS int64
	usage := requestctx.NewUsageCollector()
	// Commands deliberately outlive transport cancellation but retain request correlation.
	meta := requestctx.Metadata{RequestID: req.RequestID, Workload: "foreground", OperationID: req.RequestID}
	workCtx := requestctx.WithUsageCollector(requestctx.WithMetadata(context.Background(), meta), usage)
	measured := &measuredResponder{Responder: responder, status: "not_attempted"}
	responder = measured
	fields := func() []config.Field {
		f := append(requestctx.LogFields(workCtx), []config.Field{
			config.F("request_id", req.RequestID), config.F("request_kind", kind),
			config.F("prompt_type", promptType), config.F("image_count", len(req.Images)), config.F("model_image_count", len(decision.Images)),
			config.F("command_name", commandName),
		}...)
		if userID != "" {
			f = append(f, config.F("user_id", userID))
		}
		return f
	}
	defer func() {
		e := usage.ExecutionSnapshot()
		toolExecutionCount, toolBlockedCount = e.ToolExecutionCount, e.BlockedCount
		if e.Model != "" {
			model = e.Model
		}
		if e.PersistenceStatus != "" {
			persistenceStatus = e.PersistenceStatus
		}
		if e.ResponseKind != "" {
			responseKind = e.ResponseKind
		}
		if !isExecutionComplete {
			persistenceStatus = "unknown"
		}
		status := executionStatus
		if outcome.Reason == "request_canceled" {
			executionStatus, status = "canceled", "ok"
			responseKind = "canceled"
			if errors.Is(outcome.Err, broker.ErrAgentWorkCanceled) {
				reasonCode = "stop"
			} else {
				reasonCode = "shutdown"
			}
		}
		if outcome.Reason == "invalid_principal" || outcome.Reason == "user_banned" {
			executionStatus, status = "rejected", "rejected"
			reasonCode = outcome.Reason
		}
		if outcome.Err != nil && executionStatus != "canceled" && executionStatus != "rejected" {
			status = "error"
		}
		if measured.status == "error" {
			status = "error"
		}
		s := usage.Snapshot()
		f := append(fields(), config.F("duration_ms", max(int64(0), time.Since(req.ReceivedAt).Milliseconds())),
			config.F("record_kind", "summary"), config.F("is_admitted", isAdmitted), config.F("is_execution_complete", isExecutionComplete),
			config.F("request_tool_execution_count", toolExecutionCount), config.F("request_tool_blocked_count", toolBlockedCount),
			config.F("is_request_usage_reported", s.UsageReportedCallCount > 0),
			config.F("is_request_usage_complete", isExecutionComplete && s.ModelSubmissionCount > 0 && s.UsageCompleteCallCount == s.ModelSubmissionCount),
			config.F("request_usage_complete_call_count", s.UsageCompleteCallCount),
			config.F("request_usage_unknown_call_count", max(0, s.ModelSubmissionCount-s.UsageReportedCallCount)),
			config.F("is_request_embedding_usage_reported", s.EmbeddingUsageReportedCallCount > 0),
			config.F("is_request_embedding_usage_complete", isExecutionComplete && s.EmbeddingSubmissionCount > 0 && s.EmbeddingUsageCompleteCallCount == s.EmbeddingSubmissionCount),
			config.F("request_embedding_usage_complete_call_count", s.EmbeddingUsageCompleteCallCount),
			config.F("request_embedding_usage_unknown_call_count", max(0, s.EmbeddingSubmissionCount-s.EmbeddingUsageReportedCallCount)),
			config.F("queue_wait_ms", queueWaitMS), config.F("agent_duration_ms", agentDurationMS), config.F("delivery_duration_ms", measured.durationMS),
			config.F("execution_status", executionStatus), config.F("delivery_status", measured.status), config.F("persistence_status", persistenceStatus),
			config.F("response_kind", responseKind), config.F("status", status),
			config.F("outcome", executionStatus),
			config.F("reason_code", reasonCode),
			config.F("request_model_call_count", s.ModelCallCount), config.F("request_model_submission_count", s.ModelSubmissionCount),
			config.F("request_model_failure_count", s.ModelFailureCount), config.F("request_usage_reported_call_count", s.UsageReportedCallCount),
			config.F("request_prompt_tokens", s.PromptTokens), config.F("request_completion_tokens", s.CompletionTokens), config.F("request_total_tokens", s.TotalTokens),
			config.F("request_model_duration_ms", s.ModelDurationMS), config.F("request_embedding_call_count", s.EmbeddingCallCount),
			config.F("request_embedding_submission_count", s.EmbeddingSubmissionCount), config.F("request_embedding_failure_count", s.EmbeddingFailureCount),
			config.F("request_embedding_usage_reported_call_count", s.EmbeddingUsageReportedCallCount), config.F("request_embedding_prompt_tokens", s.EmbeddingPromptTokens),
			config.F("request_embedding_completion_tokens", s.EmbeddingCompletionTokens), config.F("request_embedding_total_tokens", s.EmbeddingTotalTokens), config.F("request_embedding_duration_ms", s.EmbeddingDurationMS))
		if model != "" {
			f = append(f, config.F("model", model))
		}
		log.Info("gateway.request.complete", "completed addressed gateway request", f...)
		if terminalErr == nil {
			terminalErr = outcome.Err
		}
		if status == "error" {
			errorField := config.ErrorField(terminalErr)
			if terminalErrorCode != "" {
				errorField = config.F("error_code", terminalErrorCode)
			}
			log.Error("gateway.request.failed", "gateway request failed", append(fields(), config.F("status", "error"), errorField)...)
		}
	}()

	switch decision.Action {
	case routing.ActionGatewayFallback:
		err := responder.SendFallback(decision.ResponseText)
		if err != nil {
			log.Debug("gateway.response.failed", "failed to send gateway fallback", config.F("request_id", req.RequestID), config.ErrorField(err))
		}
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: err}
	}
	if !req.Principal.Authenticated() {
		responseKind = "error"
		err := responder.SendAgentError("Failed to resolve account identity")
		log.Debug("gateway.account.invalid_principal", "request has no authenticated principal", config.F("request_id", req.RequestID))
		return Outcome{Action: decision.Action, Reason: "invalid_principal", Err: err}
	}
	userID = req.Principal.CanonicalUserID
	workCtx = requestctx.WithPrincipal(workCtx, req.Principal)

	if deps.Access != nil {
		isBanned, banReason, err := deps.Access.BanStatus(userID)
		if err != nil {
			executionStatus = "error"
			responseKind = "error"
			sendErr := responder.SendAgentError(config.SafeErrorText(err))
			if sendErr != nil {
				log.Debug("gateway.send.failed", "failed to send access error response", config.F("request_id", req.RequestID), config.ErrorField(sendErr))
			}
			return Outcome{Action: decision.Action, Reason: "access_check_failed", Err: err}
		}
		if isBanned {
			responseKind = "fallback"
			err := responder.SendFallback(usermanagement.BannedMessage(banReason))
			if err != nil {
				log.Debug("gateway.response.failed", "failed to send banned response", config.F("request_id", req.RequestID), config.ErrorField(err))
			}
			return Outcome{Action: decision.Action, Reason: "user_banned", Err: err}
		}
	}

	isAdmitted = true
	log.Info("gateway.request.received", "admitted gateway request", append(fields(), config.F("record_kind", "event"), config.F("is_admitted", true))...)
	if decision.Action == routing.ActionCommand {
		startedAt := time.Now()
		response := commands.Result{Text: "Unknown command: /"}
		var commandErr error
		var sendErr error
		deliveryAttempted := false
		attachmentValidationFailed := false
		if deps.Commands != nil {
			commandReq := commands.Request{
				RequestID: req.RequestID, Principal: req.Principal, ChatID: req.ChatID,
				SessionKey: req.SessionKey, DisplayName: req.DisplayName, ClientID: req.ClientID,
				Raw: decision.Prompt,
			}
			definition, _ := deps.Commands.Definition(commandName)
			var fenceTargets []string
			var resolveErr error
			if !definition.OutOfBand {
				fenceTargets, resolveErr = deps.Commands.ResolveFenceTargets(workCtx, commandReq)
			}
			executeCommand := func() error {
				queueWaitMS = time.Since(startedAt).Milliseconds()
				if resolveErr != nil {
					commandErr = resolveErr
				} else {
					response, commandErr = deps.Commands.Execute(workCtx, commandReq)
				}
				if response.Outcome.IsChanged && response.Outcome.Operation != "" {
					// IsChanged certifies an actual commit, even if later command work failed.
					mutationStatus := response.Outcome.Status
					if mutationStatus == "" {
						mutationStatus = "ok"
					}
					if commandErr != nil {
						mutationStatus = "error"
					}
					log.Info("gateway.command.mutation.complete", "completed command mutation", append(fields(),
						config.F("operation", response.Outcome.Operation), config.F("reason_code", response.Outcome.ReasonCode),
						config.F("is_changed", true), config.F("affected_count", response.Outcome.AffectedCount),
						config.F("active_canceled_count", response.Outcome.ActiveCanceledCount), config.F("queued_canceled_count", response.Outcome.QueuedCanceledCount), config.F("status", mutationStatus))...)
				}
				if commandErr != nil {
					response.Text = config.SafeErrorText(commandErr)
					response.Attachments = nil
				} else if attachments := response.Attachments; len(attachments) > 0 {
					totalBytes := 0
					for _, attachment := range attachments {
						totalBytes += len(attachment.Data)
					}
					if validateErr := response.ValidateAttachments(); validateErr != nil {
						commandErr = validateErr
						attachmentValidationFailed = true
						log.Debug("gateway.command.attachment_invalid", "command returned invalid attachments", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("attachment_count", len(attachments)), config.F("attachment_bytes", totalBytes), config.F("status", "error"))
						response.Text = config.SafeErrorText(validateErr)
						response.Attachments = nil
					} else {
						log.Debug("gateway.command.attachment_ready", "prepared command attachments", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("attachment_count", len(attachments)), config.F("attachment_bytes", totalBytes))
					}
				}
				sendErr = responder.SendCommandResponse(response)
				deliveryAttempted = true
				if response.Invalidation != nil && deps.RuntimeInvalidationBus != nil {
					deps.RuntimeInvalidationBus.Publish(*response.Invalidation)
				}
				return commandErr
			}
			if deps.Broker != nil && !definition.OutOfBand {
				if definition.UserExclusive || len(fenceTargets) > 0 {
					fenceTargets = append(fenceTargets, userID)
					commandErr = deps.Broker.RunUsersExclusive(context.Background(), fenceTargets, executeCommand)
				} else {
					commandErr = deps.Broker.RunInLane(context.Background(), req.Principal, req.SessionKey, executeCommand)
				}
			} else {
				commandErr = executeCommand()
			}
		}
		if commandErr != nil {
			fields := []config.Field{config.F("request_id", req.RequestID), config.F("user_id", userID)}
			if attachmentValidationFailed {
				fields = append(fields, config.F("failure_kind", "attachment_validation"))
			} else {
				fields = append(fields, config.ErrorField(commandErr))
			}
			log.Debug("gateway.command.failed", "command failed", fields...)
		}
		if !deliveryAttempted {
			if commandErr != nil {
				response = commands.Result{Text: config.SafeErrorText(commandErr)}
			}
			sendErr = responder.SendCommandResponse(response)
		}
		if sendErr != nil {
			log.Debug("gateway.response.failed", "failed to send command response", config.F("request_id", req.RequestID), config.ErrorField(sendErr))
		}
		status := response.Outcome.Status
		if response.Outcome.ReasonCode != "" {
			reasonCode = response.Outcome.ReasonCode
		}
		if status == "" {
			status = "ok"
		}
		if commandName == "unknown" {
			status = "rejected"
		}
		if commandErr != nil {
			status = "error"
			terminalErr = commandErr
			if errors.Is(commandErr, broker.ErrQueueFull) || errors.Is(commandErr, broker.ErrShuttingDown) {
				status = "rejected"
			}
		}
		executionStatus = status
		log.Debug("gateway.command.completed", "completed gateway command",
			config.F("request_id", req.RequestID),
			config.F("chat_id", req.ChatID),
			config.F("session_id", req.SessionKey),
			config.F("user_id", userID),
			config.F("command", commandName),
			config.F("response_chars", len(response.Text)),
			config.F("duration_ms", time.Since(startedAt).Milliseconds()),
			config.F("status", status),
		)
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: sendErr}
	}

	cleanup, err := responder.StartProcessing()
	if err != nil {
		log.Debug("gateway.processing.start_failed", "failed to start gateway processing indicator", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.F("status", "degraded"), config.ErrorField(err))
	}
	if cleanup != nil {
		defer cleanup()
	}

	log.Debug("gateway.request.prepared", "prepared gateway request",
		config.F("request_id", req.RequestID),
		config.F("chat_id", req.ChatID),
		config.F("session_id", req.SessionKey),
		config.F("user_id", userID),
		config.F("identity_assurance", req.Principal.Assurance),
		config.F("image_count", len(decision.Images)),
		config.F("is_group", req.IsGroup),
		config.F("is_mention", req.IsMention),
		config.F("is_reply", req.Reply != nil),
		config.F("prompt_chars", len(decision.Prompt)),
	)

	brokerReq := &broker.Request{
		Usage:        usage,
		Metadata:     meta,
		RequestID:    req.RequestID,
		ChatID:       req.ChatID,
		Principal:    req.Principal,
		DisplayName:  req.DisplayName,
		SessionKey:   req.SessionKey,
		IsDirect:     req.IsDirect,
		Prompt:       decision.Prompt,
		Images:       decision.Images,
		StreamFunc:   req.StreamFunc,
		ResponseChan: make(chan broker.Result, 1),
	}
	if resolver, ok := deps.Access.(interface {
		ResolvePrincipal(identity.Principal) (string, error)
	}); ok {
		brokerReq.RefreshPrincipal = func(principal identity.Principal) (identity.Principal, error) {
			resolvedUserID, err := resolver.ResolvePrincipal(principal)
			if err != nil {
				return identity.Principal{}, err
			}
			principal.CanonicalUserID = resolvedUserID
			return principal, nil
		}
	}
	submitErr := deps.Broker.Submit(brokerReq)
	if submitErr != nil {
		executionStatus, responseKind = "rejected", "fallback"
		reasonCode = "queue_full"
	}
	result := <-brokerReq.ResponseChan
	isExecutionComplete = result.ExecutionComplete
	if errors.Is(submitErr, broker.ErrShuttingDown) {
		result.Err = submitErr
	}
	queueWaitMS, agentDurationMS = result.QueueWaitMS, result.AgentDurationMS
	if result.Principal.Valid() {
		userID = result.Principal.CanonicalUserID
	}

	if result.Err != nil {
		executionStatus = "error"
		responseKind = "error"
		if errors.Is(result.Err, broker.ErrAgentWorkCanceled) || errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, broker.ErrShuttingDown) {
			if cancelResponder, ok := responder.(CancellationResponder); ok {
				if err := cancelResponder.CancelAgentResponse(); err != nil {
					log.Warn("gateway.response.cancel_cleanup_failed", "failed to clean up canceled agent response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.F("status", "degraded"), config.ErrorField(err))
				}
			}
			return Outcome{Action: decision.Action, Reason: "request_canceled", Err: result.Err}
		}
		log.Debug("gateway.response.failed", "agent processing failed", config.F("request_id", req.RequestID), config.ErrorField(result.Err))
		err := responder.SendAgentError(config.SafeErrorText(result.Err))
		if err != nil {
			log.Debug("gateway.send.failed", "failed to send agent error response", config.F("request_id", req.RequestID), config.ErrorField(err))
		}
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: result.Err}
	}

	if result.Response != nil {
		model = result.Response.Model
		toolExecutionCount, toolBlockedCount = result.Response.ToolExecutionCount, result.Response.ToolBlockedCount
		if result.Response.Kind != "" {
			responseKind = result.Response.Kind
		}
		if result.Response.PersistenceStatus != "" {
			persistenceStatus = result.Response.PersistenceStatus
		}
		if result.Response.Error != "" || responseKind == "provider_error" {
			executionStatus, responseKind = "error", "provider_error"
			terminalErrorCode = "model_failure"
		}
		if executionStatus == "ok" && responseKind != "answer" {
			executionStatus = "degraded"
		}
	}
	err = responder.SendAgentResponse(result.Response)
	if err != nil {
		log.Debug("gateway.send.failed", "failed to send agent response", config.F("request_id", req.RequestID), config.ErrorField(err))
		if deps.Compaction != nil && result.Response != nil && result.Response.SourceTurnID > 0 {
			if markErr := deps.Compaction.MarkDeliveryFailed(context.Background(), userID, result.Response.SourceTurnID); markErr != nil {
				log.Warn("session.delivery.failure_mark_failed", "failed to mark terminal response delivery failure", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("turn_id", result.Response.SourceTurnID), config.F("status", "degraded"), config.ErrorField(markErr))
			}
		}
	} else if result.Response != nil {
		log.Debug("gateway.response.sent", "sent gateway response",
			config.F("request_id", req.RequestID),
			config.F("chat_id", req.ChatID),
			config.F("session_id", req.SessionKey),
			config.F("user_id", userID),
			config.F("response_chars", len(result.Response.Response)),
			config.F("status", "ok"),
		)
		if deps.Formation != nil && result.Response.SourceTurnID > 0 {
			source := memory.FormationSource{
				RequestID: req.RequestID, SessionID: req.SessionKey,
				SessionGeneration: result.Response.SessionGeneration,
				TurnID:            result.Response.SourceTurnID, Model: result.Response.Model,
				ExtractorVersion: memory.FormationExtractorVersion,
			}
			if enqueueErr := deps.Formation.Enqueue(context.Background(), userID, source); enqueueErr != nil {
				log.Warn("user_memory.formation.job.enqueue_failed", "failed to enqueue post-turn user-memory formation", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("turn_id", result.Response.SourceTurnID), config.F("status", "degraded"), config.ErrorField(enqueueErr))
			}
		}
		if deps.Compaction != nil && result.Response.SourceTurnID > 0 {
			source := memory.FormationSource{
				RequestID: req.RequestID, SessionID: req.SessionKey,
				SessionGeneration: result.Response.SessionGeneration,
				TurnID:            result.Response.SourceTurnID, Model: result.Response.Model,
			}
			if enqueueErr := deps.Compaction.Enqueue(context.Background(), userID, source); enqueueErr != nil {
				log.Warn("session.compaction.job.enqueue_failed", "failed to enqueue session compaction planning", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("turn_id", result.Response.SourceTurnID), config.F("status", "degraded"), config.ErrorField(enqueueErr))
			}
		}
	}
	return Outcome{Action: decision.Action, Reason: decision.Reason, Err: err}
}
