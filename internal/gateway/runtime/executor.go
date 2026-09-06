package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/usermanagement"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// Execute applies shared routing policy, command handling, and broker submission.
func Execute(req Request, deps Dependencies, responder Responder) Outcome {
	log := deps.Log.Server("gateway.runtime", config.F("gateway", req.Principal.Gateway))
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

	switch decision.Action {
	case routing.ActionIgnore:
		return Outcome{Action: decision.Action, Reason: decision.Reason}
	case routing.ActionGatewayFallback:
		err := responder.SendFallback(decision.ResponseText)
		if err != nil {
			log.Error("gateway.response.failed", "failed to send gateway fallback", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
		}
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: err}
	}
	if !req.Principal.Authenticated() {
		err := responder.SendAgentError("Failed to resolve account identity")
		log.Error("gateway.account.invalid_principal", "request has no authenticated principal", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID))
		return Outcome{Action: decision.Action, Reason: "invalid_principal", Err: err}
	}
	userID := req.Principal.CanonicalUserID

	if deps.Access != nil {
		isBanned, banReason, err := deps.Access.BanStatus(userID)
		if err != nil {
			log.Error("gateway.access_check.failed", "failed to check user access", config.F("request_id", req.RequestID), config.F("user_id", userID), config.ErrorField(err))
			sendErr := responder.SendAgentError(config.SafeErrorText(err))
			if sendErr != nil {
				log.Error("gateway.send.failed", "failed to send access error response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(sendErr))
			}
			return Outcome{Action: decision.Action, Reason: "access_check_failed", Err: err}
		}
		if isBanned {
			err := responder.SendFallback(usermanagement.BannedMessage(banReason))
			if err != nil {
				log.Error("gateway.response.failed", "failed to send banned response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
			}
			return Outcome{Action: decision.Action, Reason: "user_banned", Err: err}
		}
	}

	if decision.Action == routing.ActionCommand {
		startedAt := time.Now()
		parsed, _ := commands.Parse(decision.Prompt)
		commandName := parsed.Name
		if commandName == "" {
			commandName = "unknown"
		}
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
				fenceTargets, resolveErr = deps.Commands.ResolveFenceTargets(context.Background(), commandReq)
			}
			executeCommand := func() error {
				if resolveErr != nil {
					commandErr = resolveErr
				} else {
					response, commandErr = deps.Commands.Execute(context.Background(), commandReq)
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
						log.Error("gateway.command.attachment_invalid", "command returned invalid attachments", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("attachment_count", len(attachments)), config.F("attachment_bytes", totalBytes), config.F("status", "error"))
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
			log.Error("gateway.command.failed", "command failed", fields...)
		}
		if !deliveryAttempted {
			if commandErr != nil {
				response = commands.Result{Text: config.SafeErrorText(commandErr)}
			}
			sendErr = responder.SendCommandResponse(response)
		}
		if sendErr != nil {
			log.Error("gateway.response.failed", "failed to send command response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(sendErr))
		}
		status := "ok"
		if commandErr != nil || sendErr != nil {
			status = "error"
		}
		log.Info("gateway.command.completed", "completed gateway command",
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

	log.Info("gateway.request.received", "received gateway request",
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
	deps.Broker.Submit(brokerReq)
	result := <-brokerReq.ResponseChan
	if result.Principal.Valid() {
		userID = result.Principal.CanonicalUserID
	}

	if result.Err != nil {
		if errors.Is(result.Err, broker.ErrAgentWorkCanceled) {
			if cancelResponder, ok := responder.(CancellationResponder); ok {
				if err := cancelResponder.CancelAgentResponse(); err != nil {
					log.Warn("gateway.response.cancel_cleanup_failed", "failed to clean up canceled agent response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.F("status", "degraded"), config.ErrorField(err))
				}
			}
			return Outcome{Action: decision.Action, Reason: "request_canceled", Err: result.Err}
		}
		log.Error("gateway.response.failed", "agent processing failed", config.F("request_id", req.RequestID), config.ErrorField(result.Err))
		err := responder.SendAgentError(config.SafeErrorText(result.Err))
		if err != nil {
			log.Error("gateway.send.failed", "failed to send agent error response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
		}
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: result.Err}
	}

	err = responder.SendAgentResponse(result.Response)
	if err != nil {
		log.Error("gateway.send.failed", "failed to send agent response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
		if deps.Compaction != nil && result.Response != nil && result.Response.SourceTurnID > 0 {
			if markErr := deps.Compaction.MarkDeliveryFailed(context.Background(), userID, result.Response.SourceTurnID); markErr != nil {
				log.Warn("session.delivery.failure_mark_failed", "failed to mark terminal response delivery failure", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("turn_id", result.Response.SourceTurnID), config.F("status", "degraded"), config.ErrorField(markErr))
			}
		}
	} else if result.Response != nil {
		log.Info("gateway.response.sent", "sent gateway response",
			config.F("request_id", req.RequestID),
			config.F("chat_id", req.ChatID),
			config.F("session_id", req.SessionKey),
			config.F("user_id", userID),
			config.F("response_chars", len(result.Response.Response)),
			config.F("status", "ok"),
		)
		if deps.Formation != nil && result.Response.SourceTurnID > 0 {
			source := usermemory.FormationSource{
				RequestID: req.RequestID, SessionID: req.SessionKey,
				SessionGeneration: result.Response.SessionGeneration,
				TurnID:            result.Response.SourceTurnID, Model: result.Response.Model,
				ExtractorVersion: usermemory.FormationExtractorVersion,
			}
			if enqueueErr := deps.Formation.Enqueue(context.Background(), userID, source); enqueueErr != nil {
				log.Warn("user_memory.formation.job.enqueue_failed", "failed to enqueue post-turn user-memory formation", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("turn_id", result.Response.SourceTurnID), config.F("status", "degraded"), config.ErrorField(enqueueErr))
			}
		}
		if deps.Compaction != nil && result.Response.SourceTurnID > 0 {
			source := usermemory.FormationSource{
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
