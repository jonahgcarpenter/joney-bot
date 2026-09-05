package sessionruntime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/leaseruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const (
	maximumCompactionRange    = 64
	maximumCompactionAttempts = 3
	maximumRecentTail         = 2
	promptPressurePrefix      = "session-prompt-pressure-v1"
)

// LowPriorityGate grants model capacity only while foreground work is idle.
type LowPriorityGate interface {
	TryAcquireLowPriority(context.Context) (context.Context, func(), bool)
}

// Service plans and serially executes durable session compaction jobs.
type Service struct {
	store        *usermemory.Store
	extractor    Extractor
	model        string
	budget       promptbudget.ContextBudget
	log          *config.Logger
	owner        string
	lease        time.Duration
	gate         LowPriorityGate
	planRequests chan usermemory.ActiveSessionScope
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// SetLowPriorityGate makes compaction model calls yield to foreground work.
func (s *Service) SetLowPriorityGate(gate LowPriorityGate) {
	s.gate = gate
}

// NewService constructs the post-delivery compaction service.
func NewService(store *usermemory.Store, extractor Extractor, model string, budget promptbudget.ContextBudget, log *config.Logger) *Service {
	return &Service{store: store, extractor: extractor, model: model, budget: budget, log: log, owner: fmt.Sprintf("session-compactor-%d", time.Now().UnixNano()), lease: 2 * time.Minute, planRequests: make(chan usermemory.ActiveSessionScope, 128)}
}

// Start reconciles durable state and starts one serialized worker.
func (s *Service) Start(parent context.Context) {
	if s == nil || s.store == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop cancels work and waits for the worker; leases remain recoverable.
func (s *Service) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// Enqueue marks delivery and proactively plans a fixed compaction range.
func (s *Service) Enqueue(ctx context.Context, userID string, source usermemory.FormationSource) error {
	if source.TurnID <= 0 || source.SessionGeneration <= 0 {
		return fmt.Errorf("session compaction enqueue requires source turn and generation")
	}
	if err := s.store.MarkSessionTurnDelivered(ctx, userID, source.TurnID); err != nil {
		return err
	}
	select {
	case s.planRequests <- usermemory.ActiveSessionScope{UserID: userID, SessionID: source.SessionID, Generation: source.SessionGeneration}:
	default:
	}
	return nil
}

// MarkDeliveryFailed records a terminal send failure without exposing the turn.
func (s *Service) MarkDeliveryFailed(ctx context.Context, userID string, turnID int64) error {
	return s.store.MarkSessionTurnDeliveryFailed(ctx, userID, turnID)
}

func (s *Service) run(ctx context.Context) {
	defer s.wg.Done()
	if _, err := s.store.ReconcileSessionCompactionJobs(ctx, s.model, SummaryGeneratorVersion); err != nil {
		s.warn("session.compaction.job.reconcile_failed", "failed to reconcile session compaction jobs", err)
	}
	s.planActiveSessions(ctx)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case scope := <-s.planRequests:
			if _, err := s.plan(ctx, scope.UserID, scope.SessionID, scope.Generation); err != nil {
				s.warn("session.compaction.plan.failed", "failed to plan session compaction", err, config.F("user_id", scope.UserID), config.F("session_id", scope.SessionID))
			}
		case <-ticker.C:
			ticks++
			if ticks%60 == 0 {
				if _, err := s.store.ReconcileSessionCompactionJobs(ctx, s.model, SummaryGeneratorVersion); err != nil {
					s.warn("session.compaction.job.reconcile_failed", "failed to reconcile session compaction jobs", err)
				}
				s.planActiveSessions(ctx)
			}
		}
	}
}

func (s *Service) planActiveSessions(ctx context.Context) {
	scopes, err := s.store.ActiveSessionScopes(ctx, 0)
	if err != nil {
		s.warn("session.compaction.plan.scan_failed", "failed to scan active sessions", err)
		return
	}
	for _, scope := range scopes {
		if _, err := s.plan(ctx, scope.UserID, scope.SessionID, scope.Generation); err != nil {
			s.warn("session.compaction.plan.failed", "failed to plan session compaction", err, config.F("user_id", scope.UserID), config.F("session_id", scope.SessionID))
		}
	}
}

func (s *Service) plan(ctx context.Context, userID, sessionID string, generation int) (int64, error) {
	var latest usermemory.SessionSummary
	boundary := int64(0)
	latest, err := s.store.LatestSessionSummary(ctx, userID, sessionID, generation)
	if err == nil {
		boundary = latest.CoveredThroughTurnID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	target, err := s.store.SessionCompactionCampaignTarget(ctx, userID, sessionID, generation, boundary, s.model, SummaryGeneratorVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	available, err := s.store.DeliveredSessionTurnsAfter(ctx, userID, sessionID, generation, boundary, 1000)
	if err != nil {
		return 0, err
	}
	if len(available.Turns) == 0 {
		return 0, nil
	}
	if target == 0 {
		pressure, pressureErr := s.store.LatestDeliveredSessionPromptPressure(ctx, userID, sessionID, generation, promptPressureVersion(s.model, s.budget.UsableInputLimit()))
		if errors.Is(pressureErr, sql.ErrNoRows) {
			return 0, nil
		}
		if pressureErr != nil {
			return 0, pressureErr
		}
		if pressure.Tokens < pressure.Limit {
			return 0, nil
		}
		if available.TotalCount > len(available.Turns) {
			target = available.Turns[len(available.Turns)-1].ID
		} else {
			tailCount := preservedRecentTailCount(available.Turns, pressure.Limit)
			if len(available.Turns) <= tailCount {
				return 0, nil
			}
			target = available.Turns[len(available.Turns)-tailCount-1].ID
		}
	}
	coverCount := 0
	for _, turn := range available.Turns {
		if turn.ID > target {
			break
		}
		coverCount++
	}
	if coverCount > maximumCompactionRange {
		coverCount = maximumCompactionRange
	}
	if coverCount <= 0 {
		return 0, nil
	}
	var previous *usermemory.SessionSummary
	if latest.ID > 0 {
		previous = &latest
	}
	inputLimit := s.budget.UsableInputLimit()
	for coverCount > 0 {
		messages, buildErr := compactionMessages(previous, available.Turns[:coverCount], "invalid_argument_shape")
		if buildErr != nil {
			return 0, buildErr
		}
		if promptbudget.EstimateRequest(messages, []llm.Tool{sessionSummarySaveTool()}) <= inputLimit {
			break
		}
		coverCount--
	}
	if coverCount == 0 {
		from := available.Turns[0].ID
		if latest.ID > 0 {
			from = latest.CoveredFromTurnID
		}
		return s.store.RecordUncompactableSessionCompactionCampaign(ctx, userID, sessionID, generation, from, available.Turns[0].ID, target, s.model, SummaryGeneratorVersion)
	}
	from := available.Turns[0].ID
	if latest.ID > 0 {
		from = latest.CoveredFromTurnID
	}
	through := available.Turns[coverCount-1].ID
	return s.store.EnqueueSessionCompactionCampaignJob(ctx, userID, sessionID, generation, from, through, target, s.model, SummaryGeneratorVersion)
}

func preservedRecentTailCount(chronological []usermemory.SessionTurn, inputLimit int) int {
	budget := inputLimit / 4
	if budget < 2000 {
		budget = 2000
	}
	if budget > 8000 {
		budget = 8000
	}
	if budget > inputLimit {
		budget = inputLimit
	}
	total := 0
	count := 0
	for i := len(chronological) - 1; i >= 0 && count < maximumRecentTail; i-- {
		size := promptbudget.EstimateRequest(usermemory.SessionTurnMessages(chronological[i]), nil)
		if total+size > budget {
			break
		}
		total += size
		count++
	}
	return count
}

func promptPressureVersion(model string, inputLimit int) string {
	return fmt.Sprintf("%s:%s:%d", promptPressurePrefix, strings.TrimSpace(model), inputLimit)
}

func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		claimOwner := fmt.Sprintf("%s-%d", s.owner, time.Now().UnixNano())
		job, err := s.store.ClaimSessionCompactionJob(ctx, claimOwner, s.lease, s.model, SummaryGeneratorVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			s.warn("session.compaction.job.claim_failed", "failed to claim session compaction job", err)
			return
		}
		err = s.process(ctx, &job)
		if err != nil {
			if errors.Is(err, errLowPriorityUnavailable) {
				if deferErr := s.store.DeferSessionCompactionJob(context.Background(), job, time.Second); deferErr != nil {
					s.warn("session.compaction.job.defer_failed", "failed to defer preempted session compaction job", deferErr, config.F("job_id", job.ID))
				}
				return
			}
			if errors.Is(err, usermemory.ErrModelSubmissionBudgetExhausted) {
				if retryErr := s.store.RetrySessionCompactionJob(context.Background(), job, "model_submission_budget_exhausted"); retryErr != nil {
					s.warn("session.compaction.job.retry_failed", "failed to terminally close exhausted session compaction job", retryErr, config.F("job_id", job.ID))
				}
				continue
			}
			code := compactionErrorCode(err)
			fields := []config.Field{config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("session_id", job.SessionID), config.F("session_generation", job.SessionGeneration), config.F("model", job.Model), config.F("generator_version", job.GeneratorVersion), config.F("attempt_count", job.AttemptCount), config.F("invalid_output_retry_count", job.InvalidOutputRetryCount), config.F("model_submission_count", job.ModelSubmissionCount), config.F("redrive_count", job.RedriveCount), config.F("error_code", code)}
			if errors.Is(err, errInvalidCompactionOutput) {
				if job.InvalidOutputRetryCount == 0 && job.ModelSubmissionCount < usermemory.DurableModelSubmissionLimit {
					if retryErr := s.store.RetryInvalidSessionCompactionJob(context.Background(), job, code); retryErr != nil {
						s.warn("session.compaction.job.retry_failed", "failed to schedule session compaction structured-output retry", retryErr, fields...)
					} else {
						s.warn("session.compaction.job.structural_retry", "session compaction invalid output will retry once", err, append(fields, config.F("status", "retry"))...)
					}
					continue
				}
				if skipErr := s.store.SkipSessionCompactionJob(context.Background(), job, code); skipErr != nil {
					s.warn("session.compaction.job.skip_failed", "failed to skip invalid session compaction output", skipErr, fields...)
				} else {
					s.warn("session.compaction.job.structural_skipped", "session compaction invalid output exhausted its retry", err, fields...)
				}
				continue
			}
			if errors.Is(err, errPermanentProvider) || errors.Is(err, errTerminalCompaction) {
				if skipErr := s.store.SkipSessionCompactionJob(context.Background(), job, code); skipErr != nil {
					s.warn("session.compaction.job.skip_failed", "failed to terminally skip session compaction job", skipErr, fields...)
				} else {
					s.warn("session.compaction.job.skipped", "session compaction job cannot succeed unchanged", err, fields...)
				}
				continue
			}
			if retryErr := s.store.RetrySessionCompactionJob(context.Background(), job, code); retryErr != nil {
				s.warn("session.compaction.job.retry_failed", "failed to retry session compaction job", retryErr, fields...)
				continue
			}
			if job.AttemptCount >= maximumCompactionAttempts {
				s.warn("session.compaction.job.dead", "session compaction job exhausted immediate retries", err, append(fields, config.F("status", "degraded"))...)
			} else {
				s.warn("session.compaction.job.retry", "session compaction job will retry", err, append(fields, config.F("status", "retry"))...)
			}
			continue
		}
		_, _ = s.plan(ctx, job.UserID, job.SessionID, job.SessionGeneration)
	}
}

func (s *Service) process(ctx context.Context, job *usermemory.SessionCompactionJob) error {
	artifact, err := s.store.SessionCompactionArtifact(ctx, *job)
	if errors.Is(err, sql.ErrNoRows) {
		artifact, err = s.generateArtifact(ctx, job)
		if err != nil {
			return err
		}
		if err := s.store.SaveSessionCompactionArtifact(ctx, *job, artifact); err != nil {
			return err
		}
	} else if err != nil {
		return terminalCompaction("persisted_artifact_invalid")
	}
	summary, err := s.store.PublishSessionSummary(ctx, *job)
	if err != nil {
		return err
	}
	if err := s.store.CompleteSessionCompactionJob(ctx, *job, false); err != nil {
		return err
	}
	if s.log != nil {
		s.log.Server("session.compaction").Info("session.compaction.complete", "completed session compaction", config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("session_id", job.SessionID), config.F("covered_turn_count", len(summary.SourceTurnIDs)), config.F("model", job.Model), config.F("generator_version", job.GeneratorVersion), config.F("attempt_count", job.AttemptCount), config.F("invalid_output_retry_count", job.InvalidOutputRetryCount), config.F("redrive_count", job.RedriveCount), config.F("status", "ok"))
	}
	return nil
}

func (s *Service) generateArtifact(ctx context.Context, job *usermemory.SessionCompactionJob) (usermemory.SummaryArtifact, error) {
	previous, turns, err := s.newTurnsForJob(ctx, *job)
	if err != nil {
		return usermemory.SummaryArtifact{}, err
	}
	extractParent := ctx
	release := func() {}
	if s.gate != nil {
		var acquired bool
		extractParent, release, acquired = s.gate.TryAcquireLowPriority(ctx)
		if !acquired {
			return usermemory.SummaryArtifact{}, errLowPriorityUnavailable
		}
	}
	defer release()
	if extractParent.Err() != nil {
		return usermemory.SummaryArtifact{}, errLowPriorityUnavailable
	}
	var artifact usermemory.SummaryArtifact
	renewedJob := *job
	submissionReserved := false
	err = leaseruntime.Run(extractParent, s.lease,
		func(renewCtx context.Context) error {
			leaseUntil, renewErr := s.store.RenewSessionCompactionJobLease(renewCtx, renewedJob, s.lease)
			if renewErr == nil {
				renewedJob.LeaseUntil = leaseUntil
			}
			return renewErr
		},
		func(workCtx context.Context) error {
			extractCtx := requestctx.WithMetadata(workCtx, requestctx.Metadata{RequestID: fmt.Sprintf("session-compaction:%d", job.ID), SessionID: job.SessionID, SessionGeneration: job.SessionGeneration, Model: job.Model})
			extractCtx = requestctx.WithPrincipal(extractCtx, identity.Principal{CanonicalUserID: job.UserID, Gateway: "session_compaction", ExternalID: job.UserID, Assurance: identity.AssuranceSelfAsserted})
			var compactErr error
			count, reserveErr := s.store.ReserveSessionCompactionModelSubmission(workCtx, renewedJob)
			if reserveErr != nil {
				return reserveErr
			}
			renewedJob.ModelSubmissionCount = count
			submissionReserved = true
			artifact, compactErr = s.extractor.Compact(extractCtx, previous, turns, renewedJob.CorrectiveErrorCode)
			return compactErr
		},
	)
	*job = renewedJob
	wasPreempted := extractParent.Err() != nil && ctx.Err() == nil
	release()
	release = func() {}
	if wasPreempted {
		if submissionReserved {
			if refundErr := s.store.RefundSessionCompactionModelSubmission(context.Background(), renewedJob); refundErr != nil {
				return usermemory.SummaryArtifact{}, refundErr
			}
			job.ModelSubmissionCount--
		}
		return usermemory.SummaryArtifact{}, errLowPriorityUnavailable
	}
	if err != nil {
		return usermemory.SummaryArtifact{}, err
	}
	artifact.GenerationModel = job.Model
	artifact.GeneratorVersion = job.GeneratorVersion
	return artifact, nil
}

func (s *Service) newTurnsForJob(ctx context.Context, job usermemory.SessionCompactionJob) (*usermemory.SessionSummary, []usermemory.SessionTurn, error) {
	var previous *usermemory.SessionSummary
	boundary := job.CoveredFromTurnID - 1
	prior, err := s.store.SessionSummaryBefore(ctx, job.UserID, job.SessionID, job.SessionGeneration, job.CoveredThroughTurnID)
	if err == nil {
		previous = &prior
		boundary = prior.CoveredThroughTurnID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	turns, err := s.store.DeliveredSessionTurnsRange(ctx, job.UserID, job.SessionID, job.SessionGeneration, boundary+1, job.CoveredThroughTurnID)
	if err != nil {
		return nil, nil, err
	}
	if len(turns) == 0 {
		return nil, nil, terminalCompaction("empty_delivered_range")
	}
	return previous, turns, nil
}

func (s *Service) warn(event, message string, err error, fields ...config.Field) {
	if s.log == nil {
		return
	}
	hasStatus := false
	for _, field := range fields {
		if field.Key == "status" {
			hasStatus = true
			break
		}
	}
	if !hasStatus {
		fields = append(fields, config.F("status", "degraded"))
	}
	fields = append(fields, config.ErrorField(err))
	s.log.Server("session.compaction").Warn(event, message, fields...)
}
