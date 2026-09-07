package formation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/extraction"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/lease"
)

func (s *Service) processAssessment(ctx context.Context, job *memory.FormationJob) error {
	input, err := s.store.FormationAssessmentInput(ctx, *job)
	if err != nil {
		return err
	}
	artifact, err := s.store.FormationJobArtifact(ctx, *job)
	if err != nil {
		return err
	}
	var batch memory.AssessmentBatch
	var projectionFields []config.Field
	if artifact != "" {
		batch, err = extraction.DecodeAssessmentBatch([]byte(artifact), input)
		if err != nil {
			return errors.Join(errPermanentExtraction, err)
		}
	} else {
		extractor, ok := s.extractor.(AssessmentExtractor)
		if !ok {
			return errors.Join(errPermanentExtraction, fmt.Errorf("assessment extractor is unavailable"))
		}
		parent := ctx
		release := func() {}
		if s.gate != nil {
			var acquired bool
			parent, release, acquired = s.gate.TryAcquireLowPriority(ctx)
			if !acquired {
				return errBackgroundPreempted
			}
		}
		defer release()
		renewed := *job
		var mu sync.Mutex
		reserved := false
		err = lease.Run(parent, s.jobLease, func(renewCtx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			until, err := s.store.RenewFormationJobLease(renewCtx, renewed, s.jobLease)
			if err == nil {
				renewed.LeaseUntil = until
			}
			return err
		}, func(workCtx context.Context) error {
			mu.Lock()
			count, err := s.store.ReserveFormationModelSubmission(workCtx, renewed)
			if err == nil {
				renewed.ModelSubmissionCount = count
				reserved = true
			}
			mu.Unlock()
			if err != nil {
				return err
			}
			workCtx = extraction.WithAssessmentMetrics(workCtx, func(metrics extraction.AssessmentMetrics) {
				projectionFields = []config.Field{config.F("input_tokens", metrics.InputTokens), config.F("omitted_text_chars", metrics.OmittedTextChars), config.F("omitted_context_count", metrics.OmittedContextCount), config.F("omitted_observation_count", metrics.OmittedObservationCount), config.F("omitted_memory_count", metrics.OmittedMemoryCount), config.F("omitted_suppression_count", metrics.OmittedSuppressionCount), config.F("omitted_staged_count", metrics.OmittedStagedCount)}
				if s.log != nil {
					s.log.Server("user_memory.formation").Info("user_memory.formation.input.projected", "assessment input projected", append(projectionFields, config.F("record_kind", "measurement"), config.F("status", "ok"))...)
				}
			})
			batch, err = extractor.ExtractAssessment(workCtx, input, job.CorrectiveErrorCode)
			return err
		})
		// Run joins renewal before exposing the final exact token to bookkeeping.
		*job = renewed
		if parent.Err() != nil && ctx.Err() == nil {
			if err != nil && !errors.Is(err, context.Canceled) {
				s.warn("user_memory.formation.job.preemption_error", "independent formation error during preemption", err)
			}
			if reserved {
				// Foreground preemption deliberately refunds even remotely accepted work.
				if err := s.store.RefundFormationModelSubmission(context.Background(), *job); err != nil {
					return err
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
		payload, err := json.Marshal(batch)
		if err != nil {
			return &extraction.InvalidOutputError{Code: "invalid_batch_shape"}
		}
		// Validate injected extractors too, before freezing a non-replaceable artifact.
		batch, err = extraction.DecodeAssessmentBatch(payload, input)
		if err != nil {
			return err
		}
		payload, err = memory.MarshalAssessmentBatchArtifact(batch)
		if err != nil {
			return &extraction.InvalidOutputError{Code: "invalid_assessment"}
		}
		if err := s.store.SaveFormationJobArtifact(ctx, *job, string(payload)); err != nil {
			return err
		}
	}
	result, err := s.store.ApplyMemoryAssessment(ctx, *job, batch)
	if err != nil {
		return err
	}
	s.resultFields = append(assessmentResultFields(result), config.F("is_replay", artifact != ""), config.F("input_turn_count", 1+len(input.Context)), config.F("candidate_count", len(batch.Items)))
	s.resultFields = append(s.resultFields, projectionFields...)
	return nil
}

func assessmentResultFields(result memory.AssessmentResult) []config.Field {
	return []config.Field{config.F("observation_count", result.ObservationCount), config.F("published_outcome_count", result.PublishedCount), config.F("reinforced_count", result.ReinforcedCount), config.F("retired_count", result.RetiredCount), config.F("rejected_count", result.RejectedCount)}
}
