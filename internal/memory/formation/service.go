package formation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/lease"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const (
	formationPollInterval   = time.Second
	formationJobLease       = 5 * time.Minute
	formationMaxAttempts    = 5
	invalidOutputMaxRetries = 1
)

var errBackgroundPreempted = errors.New("background model work preempted by foreground traffic")

// LowPriorityGate grants model capacity only while foreground work is idle.
type LowPriorityGate interface {
	TryAcquireLowPriority(context.Context) (context.Context, func(), bool)
}

// Service owns the durable post-turn formation worker.
type Service struct {
	store     *memory.Store
	extractor Extractor
	log       *config.Logger
	model     string
	jobLease  time.Duration
	gate      LowPriorityGate
	notify    chan struct{}
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	// Set only on the attempt-local worker after processing succeeds, and emitted
	// only after CompleteFormationJob. Published outcomes may reference old rows.
	resultFields []config.Field
}

// NewService creates a serialized formation worker.
func NewService(store *memory.Store, extractor Extractor, model string, log *config.Logger) *Service {
	return &Service{store: store, extractor: extractor, model: model, jobLease: formationJobLease, log: log, notify: make(chan struct{}, 1)}
}

// SetLowPriorityGate makes extraction yield to foreground model work.
func (s *Service) SetLowPriorityGate(gate LowPriorityGate) {
	s.gate = gate
}

// Start begins startup recovery and polling.
func (s *Service) Start(parent context.Context) {
	if s == nil || s.store == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop releases unfinished leases for restart and waits for the worker.
func (s *Service) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// Enqueue records post-delivery extraction without running it inline.
func (s *Service) Enqueue(ctx context.Context, userID string, source memory.FormationSource) error {
	if err := s.store.MarkFormationEligible(ctx, userID, source.TurnID); err != nil {
		return err
	}
	_, _, agentSaveErr := s.store.EnqueueAgentSaveFormationJob(ctx, source, userID)
	var backgroundErr error
	if _, ok := s.extractor.(AssessmentExtractor); ok {
		_, _, backgroundErr = s.store.EnqueueAssessmentFormationJob(ctx, source, userID)
	} else if _, ok := s.extractor.(PatternExtractor); ok {
		_, _, backgroundErr = s.store.EnqueuePatternFormationJob(ctx, source, userID)
	} else {
		_, backgroundErr = s.store.EnqueueFormationJob(ctx, source, userID)
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return errors.Join(agentSaveErr, backgroundErr)
}

func (s *Service) run(ctx context.Context) {
	defer s.wg.Done()
	if s.log != nil {
		s.log.Server("user_memory.formation").Info("user_memory.formation.worker.started", "formation worker started", config.F("workload", "formation"))
		defer s.log.Server("user_memory.formation").Info("user_memory.formation.worker.stopped", "formation worker stopped", config.F("workload", "formation"))
	}
	ticker := time.NewTicker(formationPollInterval)
	defer ticker.Stop()
	s.reconcile(ctx)
	ticks := 0
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ticks++
			if ticks%60 == 0 {
				s.reconcile(ctx)
			}
		case <-s.notify:
		}
	}
}

func (s *Service) reconcile(ctx context.Context) {
	if count, err := s.store.ReconcileAgentSaveFormationJobs(ctx, s.model); err != nil {
		s.warn("user_memory.formation.job.reconcile_failed", "failed to reconcile agent-save formation jobs", err, config.F("formation_purpose", memory.FormationPurposeAgentSave))
	} else if count > 0 && s.log != nil {
		s.log.Server("user_memory.formation").Info("user_memory.formation.job.reconciled", "reconciled agent-save formation jobs", config.F("job_count", count), config.F("formation_purpose", memory.FormationPurposeAgentSave), config.F("status", "ok"))
	}
	var count int64
	var err error
	if _, ok := s.extractor.(AssessmentExtractor); ok {
		count, err = s.store.ReconcileAssessmentFormationJobs(ctx, s.model)
	} else if _, ok := s.extractor.(PatternExtractor); ok {
		count, err = s.store.ReconcilePatternFormationJobs(ctx, s.model)
	} else {
		count, err = s.store.ReconcileFormationJobs(ctx, s.model, memory.FormationExtractorVersion)
	}
	if err != nil {
		s.warn("user_memory.formation.job.reconcile_failed", "failed to reconcile user-memory formation jobs", err)
	} else if count > 0 && s.log != nil {
		s.log.Server("user_memory.formation").Info("user_memory.formation.job.reconciled", "reconciled user-memory formation jobs", config.F("job_count", count), config.F("formation_purpose", memory.FormationPurposeBackgroundPattern), config.F("status", "ok"))
	}
}

func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := s.store.ClaimFormationJob(ctx, s.jobLease)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			s.warn("user_memory.formation.job.claim_failed", "failed to claim user-memory formation job", err)
			return
		}
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		meta.ParentOperationID, meta.OperationID = meta.OperationID, rand.Text()
		meta.Workload, meta.JobID, meta.RequestID, meta.Model = "formation", job.ID, job.RequestID, job.Model
		jobCtx := requestctx.WithMetadata(ctx, meta)
		jobCtx = requestctx.WithPrincipal(jobCtx, identity.Principal{CanonicalUserID: job.UserID})
		jobCtx = requestctx.WithUsageCollector(jobCtx, requestctx.NewUsageCollector())
		worker := &Service{store: s.store, extractor: s.extractor, model: s.model, jobLease: s.jobLease, gate: s.gate, log: s.log}
		if worker.log != nil {
			worker.log = worker.log.With(requestctx.LogFields(jobCtx)...).With(config.F("job_kind", "memory_formation"), config.F("formation_purpose", job.Purpose), config.F("attempt_count", job.AttemptCount), config.F("model_submission_limit", memory.DurableModelSubmissionLimit), config.F("source_turn_id", job.TurnID))
			if job.Purpose == memory.FormationPurposeAgentSave {
				worker.log = worker.log.With(config.F("attempt_limit", formationMaxAttempts), config.F("model_submission_limit", 0))
			}
			worker.log.Server("user_memory.formation").Info("user_memory.formation.job.started", "formation job attempt started", config.F("model_submission_count", job.ModelSubmissionCount), config.F("status", "ok"))
		}
		s := worker
		err = s.process(jobCtx, &job)
		if s.log != nil {
			s.log = s.log.With(config.F("duration_ms", time.Since(started).Milliseconds()), config.F("model_submission_count", job.ModelSubmissionCount), config.F("invalid_output_retry_count", job.InvalidOutputRetryCount))
		}
		if err != nil {
			if errors.Is(err, errBackgroundPreempted) || errors.Is(err, context.Canceled) {
				if deferErr := s.store.DeferFormationJob(context.Background(), job, time.Second); deferErr != nil {
					s.warn("user_memory.formation.job.defer_failed", "failed to defer preempted user-memory formation job", deferErr, config.F("job_id", job.ID), config.F("user_id", job.UserID))
				} else if s.log != nil {
					s.log.Server("user_memory.formation").Info("user_memory.formation.job.deferred", "formation work deferred", config.F("outcome", "deferred"), config.F("attempt_count", max(job.AttemptCount-1, 0)), config.F("model_submission_count", job.ModelSubmissionCount), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", "ok"))
				}
				return
			}
			if errors.Is(err, memory.ErrModelSubmissionBudgetExhausted) {
				if state, retryErr := s.store.RetryFormationJob(context.Background(), job, "model_submission_budget_exhausted", formationMaxAttempts); retryErr != nil {
					s.warn("user_memory.formation.job.complete_failed", "failed to terminally close exhausted user-memory formation job", retryErr, config.F("job_id", job.ID), config.F("user_id", job.UserID))
				} else if s.log != nil {
					s.log.Server("user_memory.formation").Info("user_memory.formation.job.budget_exhausted", "formation submission budget exhausted", config.F("job_state", state), config.F("model_submission_count", job.ModelSubmissionCount), config.F("model_submission_limit", memory.DurableModelSubmissionLimit), config.F("status", "degraded"))
				}
				continue
			}
			if errors.Is(err, errInvalidOutput) {
				code := errorCode(err)
				fields := []config.Field{config.F("attempt_count", job.AttemptCount), config.F("invalid_output_retry_count", job.InvalidOutputRetryCount), config.F("model_submission_count", job.ModelSubmissionCount), config.F("model_submission_limit", memory.DurableModelSubmissionLimit), config.F("reason_code", code)}
				if job.InvalidOutputRetryCount < invalidOutputMaxRetries && job.ModelSubmissionCount < memory.DurableModelSubmissionLimit {
					if retryErr := s.store.RetryInvalidFormationJob(context.Background(), job, code); retryErr != nil {
						s.warn("user_memory.formation.job.retry_failed", "failed to retry invalid user-memory formation output", retryErr, fields...)
					} else {
						s.warn("user_memory.formation.job.invalid_output_retry", "user-memory formation invalid output will retry once", err, append(fields, config.F("status", "retry"))...)
					}
					continue
				}
				if skipErr := s.store.SkipFormationJob(context.Background(), job, code); skipErr != nil {
					s.warn("user_memory.formation.job.complete_failed", "failed to terminally skip invalid user-memory formation output", skipErr, fields...)
				} else {
					s.warn("user_memory.formation.job.skipped", "user-memory formation invalid output exhausted its retry", err, fields...)
				}
				continue
			}
			if errors.Is(err, errPermanentExtraction) {
				if skipErr := s.store.SkipFormationJob(context.Background(), job, errorCode(err)); skipErr != nil {
					s.warn("user_memory.formation.job.complete_failed", "failed to terminally skip user-memory formation job", skipErr, config.F("job_id", job.ID), config.F("user_id", job.UserID))
				} else {
					s.warn("user_memory.formation.job.skipped", "user-memory formation job returned invalid structured output", err, config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("attempt_count", job.AttemptCount))
				}
				continue
			}
			state, retryErr := s.store.RetryFormationJob(context.Background(), job, errorCode(err), formationMaxAttempts)
			if retryErr != nil {
				s.warn("user_memory.formation.job.retry_failed", "failed to release user-memory formation job lease", retryErr, config.F("job_id", job.ID), config.F("user_id", job.UserID))
				continue
			}
			event, message, status := "user_memory.formation.job.retry", "user-memory formation job will retry", "retry"
			if state == "dead" {
				event, message, status = "user_memory.formation.job.dead", "user-memory formation job exhausted immediate retries", "degraded"
			}
			s.warn(event, message, err,
				config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("attempt_count", job.AttemptCount), config.F("job_state", state), config.F("status", status))
			continue
		}
		if err := s.store.CompleteFormationJob(context.Background(), job, false); err != nil {
			s.warn("user_memory.formation.job.complete_failed", "failed to complete user-memory formation job", err, config.F("job_id", job.ID))
		} else if s.log != nil {
			s.log.Server("user_memory.formation").Info("user_memory.formation.job.complete", "formation job committed", append(s.resultFields, config.F("record_kind", "summary"), config.F("job_state", "succeeded"), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", "ok"))...)
		}
	}
}

func (s *Service) process(ctx context.Context, job *memory.FormationJob) error {
	s.resultFields = nil
	started := time.Now()
	if err := s.store.ValidateFormationJobLease(ctx, *job); err != nil {
		return err
	}
	turn, err := s.store.SessionTurnByID(ctx, job.UserID, job.TurnID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.Join(errPermanentExtraction, fmt.Errorf("memory formation source turn is unavailable"))
		}
		return err
	}
	if err := s.store.ValidateFormationJobLease(ctx, *job); err != nil {
		return err
	}
	if job.Purpose == memory.FormationPurposeAgentSave {
		return s.processAgentSave(ctx, job, turn)
	}
	if job.ExtractorVersion == memory.AssessmentExtractorVersion {
		return s.processAssessment(ctx, job)
	}
	if job.ExtractorVersion == memory.PatternExtractorVersion {
		return s.processPattern(ctx, job)
	}
	publishedCount := 0
	proposedCount := 0
	approvedCount := 0
	rejectedCount := 0
	validationFailedCount := 0
	extracted := memory.MemorySaveBatch{}
	artifact, artifactErr := s.store.FormationJobArtifact(ctx, *job)
	if artifactErr != nil {
		return artifactErr
	}
	if artifact != "" {
		var itemErrors []memory.MemorySaveItemError
		extracted, itemErrors, err = memory.DecodeMemorySaveBatchJSON([]byte(artifact))
		if err != nil {
			return errors.Join(errPermanentExtraction, fmt.Errorf("decode persisted memory formation artifact: %w", err))
		}
		if len(itemErrors) > 0 && len(extracted.Memories) == 0 {
			return errors.Join(errPermanentExtraction, fmt.Errorf("decode persisted memory formation artifact: all %d candidates were malformed", len(itemErrors)))
		}
	} else if s.extractor != nil {
		extractParent := ctx
		release := func() {}
		if s.gate != nil {
			var acquired bool
			extractParent, release, acquired = s.gate.TryAcquireLowPriority(ctx)
			if !acquired {
				return errBackgroundPreempted
			}
		}
		defer release()
		renewedJob := *job
		var jobMu sync.Mutex
		submissionReserved := false
		err = lease.Run(extractParent, s.jobLease,
			func(renewCtx context.Context) error {
				jobMu.Lock()
				defer jobMu.Unlock()
				leaseUntil, renewErr := s.store.RenewFormationJobLease(renewCtx, renewedJob, s.jobLease)
				if renewErr == nil {
					renewedJob.LeaseUntil = leaseUntil
				}
				return renewErr
			},
			func(workCtx context.Context) error {
				meta := requestctx.MetadataFromContext(workCtx)
				meta.RequestID, meta.SessionID, meta.Model, meta.CurrentUserText = job.RequestID, job.SessionID, job.Model, turn.UserText
				meta.Workload, meta.JobID = "formation", job.ID
				extractCtx := requestctx.WithMetadata(workCtx, meta)
				extractCtx = requestctx.WithPrincipal(extractCtx, identity.Principal{CanonicalUserID: job.UserID})
				var extractErr error
				jobMu.Lock()
				count, reserveErr := s.store.ReserveFormationModelSubmission(workCtx, renewedJob)
				if reserveErr != nil {
					jobMu.Unlock()
					return reserveErr
				}
				renewedJob.ModelSubmissionCount = count
				jobMu.Unlock()
				submissionReserved = true
				extracted, extractErr = s.extractor.Extract(extractCtx, turn, job.CorrectiveErrorCode)
				return extractErr
			},
		)
		*job = renewedJob
		wasPreempted := extractParent.Err() != nil && ctx.Err() == nil
		release()
		release = func() {}
		if wasPreempted {
			if err != nil && !errors.Is(err, context.Canceled) {
				s.warn("user_memory.formation.job.preemption_error", "independent formation error during preemption", err)
			}
			if submissionReserved {
				if refundErr := s.store.RefundFormationModelSubmission(context.Background(), renewedJob); refundErr != nil {
					return refundErr
				}
				job.ModelSubmissionCount--
				if s.log != nil {
					s.log.Server("user_memory.formation").Info("user_memory.formation.submission.refunded", "preempted formation submission refunded", config.F("refunded_submission_count", 1), config.F("model_submission_count", job.ModelSubmissionCount), config.F("status", "ok"))
				}
			}
			return errBackgroundPreempted
		}
		if err != nil {
			return err
		}
		payload, err := memory.MarshalMemorySaveBatchArtifact(extracted)
		if err != nil {
			return err
		}
		if err := s.store.SaveFormationJobArtifact(ctx, *job, string(payload)); err != nil {
			return err
		}
	}
	outcomes := s.store.SubmitMemorySaveBatch(ctx, job.UserID, turn.UserText, memory.FormationSource{
		RequestID: job.RequestID, SessionID: turn.SessionID, SessionGeneration: turn.Generation,
		TurnID: turn.ID, Model: job.Model, ExtractorVersion: job.ExtractorVersion,
	}, extracted, job)
	for _, outcome := range outcomes {
		if outcome.Operational {
			return outcome.Err
		}
		if outcome.Err != nil {
			validationFailedCount++
			continue
		}
		switch outcome.State {
		case "proposed":
			proposedCount++
		case "approved":
			approvedCount++
		case "rejected":
			rejectedCount++
		}
		if outcome.PublishedMemoryID != 0 {
			publishedCount++
		}
	}
	s.resultFields = []config.Field{config.F("is_replay", artifact != ""), config.F("input_turn_count", 1), config.F("submitted_count", extracted.SubmittedCount), config.F("candidate_count", len(extracted.Memories)), config.F("malformed_count", extracted.MalformedCount), config.F("validation_failed_count", validationFailedCount), config.F("proposed_count", proposedCount), config.F("approved_count", approvedCount), config.F("rejected_count", rejectedCount), config.F("published_outcome_count", publishedCount)}
	if s.log != nil {
		s.log.Server("user_memory.formation").Debug("user_memory.formation.extraction.complete", "completed user-memory formation extraction",
			config.F("job_id", job.ID), config.F("user_id", job.UserID),
			config.F("model", job.Model), config.F("extractor_version", job.ExtractorVersion), config.F("attempt_count", job.AttemptCount), config.F("invalid_output_retry_count", job.InvalidOutputRetryCount),
			config.F("submitted_count", extracted.SubmittedCount), config.F("candidate_count", len(extracted.Memories)), config.F("malformed_count", extracted.MalformedCount),
			config.F("validation_failed_count", validationFailedCount), config.F("proposed_count", proposedCount), config.F("approved_count", approvedCount),
			config.F("rejected_count", rejectedCount), config.F("published_count", publishedCount),
			config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", "ok"))
	}
	return nil
}

func (s *Service) processPattern(ctx context.Context, job *memory.FormationJob) error {
	window, err := s.store.FormationPatternContext(ctx, *job)
	if err != nil {
		return errors.Join(errPermanentExtraction, err)
	}
	extracted := memory.MemoryPatternBatch{}
	artifact, err := s.store.FormationJobArtifact(ctx, *job)
	if err != nil {
		return err
	}
	if artifact != "" {
		extracted, err = memory.DecodeMemoryPatternBatchJSON([]byte(artifact))
		if err != nil {
			return errors.Join(errPermanentExtraction, fmt.Errorf("decode persisted pattern artifact: %w", err))
		}
	} else {
		patternExtractor, ok := s.extractor.(PatternExtractor)
		if !ok {
			return errors.Join(errPermanentExtraction, fmt.Errorf("pattern extractor is unavailable"))
		}
		extractParent := ctx
		release := func() {}
		if s.gate != nil {
			var acquired bool
			extractParent, release, acquired = s.gate.TryAcquireLowPriority(ctx)
			if !acquired {
				return errBackgroundPreempted
			}
		}
		defer release()
		renewedJob := *job
		var jobMu sync.Mutex
		submissionReserved := false
		err = lease.Run(extractParent, s.jobLease, func(renewCtx context.Context) error {
			jobMu.Lock()
			defer jobMu.Unlock()
			leaseUntil, renewErr := s.store.RenewFormationJobLease(renewCtx, renewedJob, s.jobLease)
			if renewErr == nil {
				renewedJob.LeaseUntil = leaseUntil
			}
			return renewErr
		}, func(workCtx context.Context) error {
			meta := requestctx.MetadataFromContext(workCtx)
			meta.RequestID, meta.SessionID, meta.Model = job.RequestID, job.SessionID, job.Model
			meta.Workload, meta.JobID = "formation", job.ID
			extractCtx := requestctx.WithMetadata(workCtx, meta)
			extractCtx = requestctx.WithPrincipal(extractCtx, identity.Principal{CanonicalUserID: job.UserID})
			var extractErr error
			jobMu.Lock()
			count, reserveErr := s.store.ReserveFormationModelSubmission(workCtx, renewedJob)
			if reserveErr != nil {
				jobMu.Unlock()
				return reserveErr
			}
			renewedJob.ModelSubmissionCount = count
			jobMu.Unlock()
			submissionReserved = true
			extracted, extractErr = patternExtractor.ExtractPatterns(extractCtx, window.Turns, job.CorrectiveErrorCode)
			return extractErr
		})
		*job = renewedJob
		wasPreempted := extractParent.Err() != nil && ctx.Err() == nil
		release()
		release = func() {}
		if wasPreempted {
			if err != nil && !errors.Is(err, context.Canceled) {
				s.warn("user_memory.formation.job.preemption_error", "independent formation error during preemption", err)
			}
			if submissionReserved {
				if refundErr := s.store.RefundFormationModelSubmission(context.Background(), renewedJob); refundErr != nil {
					return refundErr
				}
				job.ModelSubmissionCount--
				if s.log != nil {
					s.log.Server("user_memory.formation").Info("user_memory.formation.submission.refunded", "preempted formation submission refunded", config.F("refunded_submission_count", 1), config.F("model_submission_count", job.ModelSubmissionCount), config.F("status", "ok"))
				}
			}
			return errBackgroundPreempted
		}
		if err != nil {
			return err
		}
		payload, err := memory.MarshalMemoryPatternBatchArtifact(extracted)
		if err != nil {
			return err
		}
		if err := s.store.SaveFormationJobArtifact(ctx, *job, string(payload)); err != nil {
			return err
		}
	}
	turns := make(map[int64]memory.StoredSessionTurn, len(window.Turns))
	for _, turn := range window.Turns {
		turns[turn.ID] = turn
	}
	acceptedPatternCount := 0
	rejectedPatternCount := 0
	candidateCount := 0
	publicationCount := 0
	observationCount := 0
	for patternIndex, pattern := range extracted.Patterns {
		observationCount += len(pattern.Observations)
		type evaluatedObservation struct {
			turn   memory.StoredSessionTurn
			output policy.CandidateOutput
		}
		evaluated := make([]evaluatedObservation, 0, len(pattern.Observations))
		hasAnchorObservation := false
		rejectionCode := ""
		for _, observation := range pattern.Observations {
			turn, ok := turns[observation.SourceTurnID]
			if !ok {
				evaluated = nil
				rejectionCode = "invalid_source_turn"
				break
			}
			output, evaluateErr := memory.EvaluatePatternObservation(turn, pattern, observation.Evidence)
			if evaluateErr != nil {
				evaluated = nil
				rejectionCode = "invalid_observation"
				break
			}
			if output.Approval == policy.ApprovalRejected {
				evaluated = nil
				rejectionCode = patternPolicyRejectionCode(output.Reason)
				break
			}
			evaluated = append(evaluated, evaluatedObservation{turn: turn, output: output})
			hasAnchorObservation = hasAnchorObservation || observation.SourceTurnID == job.TurnID
		}
		if rejectionCode == "" && len(evaluated) < 2 {
			rejectionCode = "insufficient_observations"
		}
		if rejectionCode == "" && !hasAnchorObservation {
			rejectionCode = "missing_anchor_observation"
		}
		if rejectionCode != "" {
			rejectedPatternCount++
			s.logPatternRejection(*job, patternIndex, rejectionCode)
			continue
		}
		for _, observation := range evaluated {
			_, _, err := s.store.ProposeCandidate(ctx, job.UserID, memory.CandidateProposal{
				Output: observation.output, RequireCorroboration: true,
				Source:         memory.FormationSource{SessionID: observation.turn.SessionID, SessionGeneration: observation.turn.Generation, TurnID: observation.turn.ID, Model: job.Model, ExtractorVersion: memory.PatternExtractorVersion},
				IdempotencyKey: fmt.Sprintf("pattern:%d:%s:%s", observation.turn.ID, observation.output.ClaimSlot, observation.output.ClaimValue), FormationJob: job,
			})
			if err != nil {
				return err
			}
			candidateCount++
		}
		publishedID, err := s.store.AggregatePatternCandidates(ctx, *job, evaluated[0].output.ClaimSlot, evaluated[0].output.ClaimValue)
		if err != nil {
			return err
		}
		if publishedID != 0 {
			publicationCount++
		}
		acceptedPatternCount++
	}
	s.resultFields = []config.Field{config.F("is_replay", artifact != ""), config.F("input_turn_count", len(window.Turns)), config.F("pattern_count", len(extracted.Patterns)), config.F("observation_count", observationCount), config.F("accepted_pattern_count", acceptedPatternCount), config.F("rejected_pattern_count", rejectedPatternCount), config.F("candidate_count", candidateCount), config.F("publication_count", publicationCount)}
	if s.log != nil {
		s.log.Server("user_memory.formation").Debug("user_memory.formation.pattern.complete", "completed user-memory pattern formation",
			config.F("job_id", job.ID), config.F("user_id", job.UserID),
			config.F("model", job.Model), config.F("extractor_version", job.ExtractorVersion), config.F("pattern_count", len(extracted.Patterns)),
			config.F("accepted_pattern_count", acceptedPatternCount), config.F("rejected_pattern_count", rejectedPatternCount), config.F("candidate_count", candidateCount), config.F("status", "ok"))
	}
	return nil
}

func (s *Service) logPatternRejection(job memory.FormationJob, patternIndex int, reasonCode string) {
	if s.log == nil {
		return
	}
	s.log.Server("user_memory.formation").Debug("user_memory.formation.pattern.rejected", "rejected user-memory pattern before candidate insertion",
		config.F("job_id", job.ID), config.F("user_id", job.UserID),
		config.F("pattern_index", patternIndex), config.F("reason_code", reasonCode), config.F("status", "rejected"))
}

func patternPolicyRejectionCode(reason string) string {
	switch reason {
	case "semantic claim slot is incompatible with memory category":
		return "invalid_claim_slot"
	case "evidence is not an exact quote from normalized source user text":
		return "evidence_mismatch"
	default:
		return "policy_rejected"
	}
}

func (s *Service) processAgentSave(ctx context.Context, job *memory.FormationJob, turn memory.StoredSessionTurn) error {
	artifact, err := s.store.SessionTurnForegroundMemory(ctx, job.UserID, job.TurnID)
	if err != nil {
		return errors.Join(errPermanentExtraction, err)
	}
	if artifact.Version == 3 {
		result, err := s.store.ApplyForegroundAssessment(ctx, *job, artifact)
		if err != nil {
			return err
		}
		s.resultFields = append(assessmentResultFields(result), config.F("input_turn_count", 1), config.F("candidate_count", len(artifact.Candidates)))
		return nil
	}
	proposed, approved, rejected, invalid, published, existing := 0, 0, 0, 0, 0, 0
	for index, candidate := range artifact.Candidates {
		output, evaluateErr := candidate.Evaluate(turn.UserText)
		if evaluateErr != nil {
			invalid++
			continue
		}
		if output.Approval == policy.ApprovalRejected {
			rejected++
			continue
		}
		result, created, err := s.store.ProposeCandidate(ctx, job.UserID, memory.CandidateProposal{
			Output:         output,
			TargetMemoryID: candidate.TargetMemoryID,
			Source: memory.FormationSource{
				RequestID: job.RequestID, SessionID: turn.SessionID, SessionGeneration: turn.Generation,
				TurnID: turn.ID, Model: job.Model, ExtractorVersion: memory.AgentSaveExtractorVersion,
			},
			IdempotencyKey:      fmt.Sprintf("agent-save:%d:%d:%s", turn.ID, index, memory.AgentSaveExtractorVersion),
			SupersedesStatement: candidate.SupersedesStatement,
			FormationJob:        job,
		})
		if err != nil {
			return err
		}
		if !created {
			existing++
		}
		switch result.State {
		case "proposed":
			proposed++
		case "approved":
			approved++
		case "rejected":
			rejected++
		}
		if result.PublishedMemoryID != 0 {
			published++
		}
	}
	// Local saves have no model artifact; reused candidates identify idempotent replay.
	s.resultFields = []config.Field{config.F("is_replay", existing > 0), config.F("input_turn_count", 1), config.F("candidate_count", len(artifact.Candidates)), config.F("validation_failed_count", invalid), config.F("proposed_count", proposed), config.F("approved_count", approved), config.F("rejected_count", rejected), config.F("published_outcome_count", published), config.F("existing_candidate_count", existing)}
	return nil
}

func (s *Service) warn(event, message string, err error, fields ...config.Field) {
	if s.log == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		s.log.Server("user_memory.formation").Info("user_memory.formation.work.canceled", "formation work canceled", append(fields, config.F("workload", "formation"), config.F("outcome", "canceled"), config.F("status", "ok"))...)
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
	fields = append(fields, config.F("workload", "formation"), config.ErrorField(err))
	s.log.Server("user_memory.formation").Warn(event, message, fields...)
}

func errorCode(err error) string {
	if err == nil {
		return "unknown"
	}
	if code, ok := invalidOutputCode(err); ok {
		return code
	}
	if errors.Is(err, errPermanentExtraction) {
		var httpErr *llm.ChatHTTPError
		if errors.As(err, &httpErr) {
			return "provider_request_rejected"
		}
		return "invalid_output"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "transient_timeout"
	}
	var httpErr *llm.ChatHTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == 429 {
			return "transient_rate_limit"
		}
		return "transient_provider"
	}
	return "transient_runtime"
}
