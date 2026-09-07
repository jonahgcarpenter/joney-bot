package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func assessmentTestCandidate() ForegroundMemoryCandidate {
	return ForegroundMemoryCandidate{Statement: "The user prefers tea.", Evidence: "I prefer tea.", Category: policy.CategoryDurablePreferences, ClaimSlot: "preference.drink", ClaimValue: "tea", EvidenceType: "direct_statement", Provenance: policy.ProvenanceUserStatement, Confidence: .9, Retention: "durable", Intent: "automatic", Cardinality: "single"}
}

func assessmentTestJob(t *testing.T, s *Store, text string) FormationJob {
	t.Helper()
	turn := seedFormationTurn(t, s, "user", "assessment", text)
	if _, created, err := s.EnqueueAssessmentFormationJob(context.Background(), FormationSource{TurnID: turn, Model: "test-model"}, "user"); err != nil || !created {
		t.Fatalf("enqueue: %v %v", created, err)
	}
	job, err := s.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestAssessmentObservationAdmissionRetentionAndReplay(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	c.Retention = "observation"
	job := assessmentTestJob(t, s, c.Evidence)
	batch := AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}
	result, err := s.ApplyMemoryAssessment(ctx, job, batch)
	if err != nil || result.ObservationCount != 1 || result.PublishedCount != 0 {
		t.Fatalf("apply: %+v %v", result, err)
	}
	if result, err := s.ApplyMemoryAssessment(ctx, job, batch); err != nil || result != (AssessmentResult{}) {
		t.Fatalf("replay: %+v %v", result, err)
	}
	observations, err := s.ListMemoryObservations(ctx, "user", time.Now())
	if err != nil || len(observations) != 1 || observations[0].SourceTurnID != job.TurnID {
		t.Fatalf("observations: %+v %v", observations, err)
	}
	if d := observations[0].ExpiresAt.Sub(observations[0].ObservedAt); d != 7*24*time.Hour {
		t.Fatalf("ttl: %v", d)
	}
	if _, err := s.sql.Exec(`UPDATE session_turns SET expires_at=? WHERE id=?`, formatTime(time.Now().Add(-time.Hour)), job.TurnID); err != nil {
		t.Fatal(err)
	}
	if observations, err := s.ListMemoryObservations(ctx, "user", time.Now()); err != nil || len(observations) != 1 {
		t.Fatalf("source expiry erased evidence: %v %v", observations, err)
	}
	if _, err := s.ResetSession(ctx, "user", "assessment", time.Hour); err != nil {
		t.Fatal(err)
	}
	if observations, err := s.ListMemoryObservations(ctx, "user", time.Now()); err != nil || len(observations) != 1 {
		t.Fatalf("reset erased observation: %v %v", observations, err)
	}
	if _, err := s.ApplyMemoryAssessment(ctx, job, batch); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("stale replay: %v", err)
	}
	if _, err := s.ResetUserDataPreservingAccount(ctx, "user", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"memory_observations", "memory_assessment_receipts", "memory_assessment_inputs", "memory_suppressions"} {
		var n int
		if err := s.sql.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("forget %s: %d %v", table, n, err)
		}
	}
}

func TestAssessmentFrozenInputExactLeaseAndAtomicRollback(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	c.Retention = "observation"
	job := assessmentTestJob(t, s, c.Evidence)
	input, err := s.FormationAssessmentInput(ctx, job)
	if err != nil || input.Anchor.ID != job.TurnID || len(input.Context) != 0 {
		t.Fatalf("input: %+v %v", input, err)
	}
	if _, err := s.sql.Exec(`UPDATE durable_jobs SET artifact_payload='{}' WHERE id=?`, job.ID); err == nil {
		t.Fatal("mutable membership")
	}
	bad := c
	bad.Evidence = "not in source"
	if _, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c, bad}}); err == nil {
		t.Fatal("bad evidence accepted")
	}
	obs, err := s.ListMemoryObservations(ctx, "user", time.Now())
	if err != nil || len(obs) != 0 {
		t.Fatalf("partial observation commit: %+v %v", obs, err)
	}
	until, err := s.RenewFormationJobLease(ctx, job, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FormationAssessmentInput(ctx, job); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("old exact token: %v", err)
	}
	job.LeaseUntil = until
	frozen, err := s.FormationAssessmentInput(ctx, job)
	if err != nil || !reflect.DeepEqual(input, frozen) {
		t.Fatalf("changed frozen view: %+v %v", frozen, err)
	}
	if _, err := s.sql.Exec(`UPDATE sessions SET expires_at=? WHERE canonical_user_id='user'`, formatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("expired admitted source: %v", err)
	}
}

func TestAssessmentCorrectionRetirementRevisionAndSuppression(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	job := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	var id, revision int64
	if err := s.sql.QueryRow(`SELECT id,revision FROM memory_entries WHERE status='active'`).Scan(&id, &revision); err != nil {
		t.Fatal(err)
	}
	correction := c
	correction.Statement = "The user prefers coffee."
	correction.Evidence = "I prefer coffee now."
	correction.ClaimValue = "coffee"
	correction.Confidence = .5
	correction.Intent = "correction"
	correction.TargetMemoryID = id
	correction.ExpectedRevision = revision
	job = assessmentTestJob(t, s, correction.Evidence)
	result, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{correction}})
	if err != nil || result.PublishedCount != 1 {
		t.Fatalf("correction: %+v %v", result, err)
	}
	if err := s.CompleteFormationJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	active, err := s.ListActiveMemories(ctx, "user", time.Now())
	if err != nil || len(active) != 1 || active[0].Statement != correction.Statement || active[0].Confidence != .5 {
		t.Fatalf("coherent corrected memory: %+v %v", active, err)
	}
	id = active[0].ID
	if err := s.sql.QueryRow(`SELECT revision FROM memory_entries WHERE id=?`, id).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	retire := correction
	retire.Statement = "The coffee preference is no longer current."
	retire.Evidence = "That coffee preference is no longer current."
	retire.Intent = "retire"
	retire.TargetMemoryID = id
	retire.ExpectedRevision = revision
	job = assessmentTestJob(t, s, retire.Evidence)
	result, err = s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{retire}})
	if err != nil || result.RetiredCount != 1 {
		t.Fatalf("retirement: %+v %v", result, err)
	}
	if active, err := s.ListActiveMemories(ctx, "user", time.Now()); err != nil || len(active) != 0 {
		t.Fatalf("retirement inferred opposite: %+v %v", active, err)
	}
	rules, err := s.ListMemorySuppressions(ctx, "user")
	if err != nil || len(rules) != 0 {
		t.Fatalf("retirement became permanent suppression: %+v %v", rules, err)
	}
	ruleID, err := s.SuppressMemory(ctx, "user", id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UnsuppressMemory(ctx, "user", ruleID); err != nil || !ok {
		t.Fatalf("unsuppress: %v %v", ok, err)
	}
	legacy := evaluatedClaimCandidate(t, correction.Evidence, correction.Statement, correction.Category, correction.Provenance, policy.SensitivityLow, .99, correction.ClaimSlot, correction.ClaimValue)
	proposed, _, err := s.ProposeCandidate(ctx, "user", CandidateProposal{Output: legacy, Source: FormationSource{TurnID: job.TurnID}})
	if err != nil || proposed.PublishedMemoryID != 0 {
		t.Fatalf("old evidence resurrected after unsuppress: %+v %v", proposed, err)
	}
}

func TestObservationBoundsAndExpiryMaintenance(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	now := time.Now().UTC()
	c := assessmentTestCandidate()
	c.Retention = "observation"
	for i := 0; i < 101; i++ {
		seedFormationTurn(t, s, "user", "bounds", c.Evidence)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 101; i++ {
		c.ClaimValue = fmt.Sprint(i)
		c.Intent = "automatic"
		if i == 0 {
			c.Intent = "remember"
		}
		created, err := insertObservationTx(ctx, tx, "user", int64(i+1), c, now)
		if err != nil || !created {
			t.Fatalf("user bound %d: %v %v", i, created, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_observations`, 100)
	assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_observations WHERE source_turn_id=1 AND intent='remember'`, 1)
	assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_observations WHERE source_turn_id=2`, 0)
	if _, err := s.cleanupExpiredSessions(ctx, now.Add(8*24*time.Hour), config.DefaultRetentionPolicy()); err != nil {
		t.Fatal(err)
	}
	obs, err := s.ListMemoryObservations(ctx, "user", now.Add(8*24*time.Hour))
	if err != nil || len(obs) != 0 {
		t.Fatalf("expired observations served: %d %v", len(obs), err)
	}
}

func TestAssessmentStrictArtifactAndImmutableResult(t *testing.T) {
	c := assessmentTestCandidate()
	batch := AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}
	payload, err := MarshalAssessmentBatchArtifact(batch)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{string(payload) + ` {}`, strings.Replace(string(payload), `"version":1`, `"version":2`, 1), `{"version":1,"items":[],"unknown":true}`} {
		if _, err := DecodeAssessmentBatchJSON([]byte(data)); err == nil {
			t.Fatal("invalid artifact accepted")
		}
	}
	s := newFormationTestStore(t)
	job := assessmentTestJob(t, s, c.Evidence)
	if err := s.SaveFormationJobArtifact(context.Background(), job, string(payload)); err != nil {
		t.Fatal(err)
	}
	batch.Items[0].Confidence = .8
	if _, err := s.ApplyMemoryAssessment(context.Background(), job, batch); err == nil {
		t.Fatal("different result replaced immutable artifact")
	}
}

func TestForegroundAssessmentAndLaterFrozenObservationMembership(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	c.Retention = "observation"
	output, err := c.Evaluate(c.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := s.ResolveSessionProfile(ctx, "user", "foreground", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "user", SessionID: "foreground", Generation: profile.Generation, UserText: c.Evidence, AssistantText: "Noted.", History: EmptyToolHistory(), TTL: time.Hour, Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "test"}, Staged: []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: output, Retention: "observation", Intent: "automatic", Cardinality: "single", TTLDays: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueAgentSaveFormationJob(ctx, FormationSource{TurnID: turn.ID}, "user"); err == nil {
		t.Fatal("pending source admitted")
	}
	if err := s.MarkFormationEligible(ctx, "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueAgentSaveFormationJob(ctx, FormationSource{TurnID: turn.ID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimFormationJob(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.SessionTurnForegroundMemory(ctx, "user", turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyForegroundAssessment(ctx, job, artifact)
	if err != nil || result.ObservationCount != 1 {
		t.Fatalf("foreground: %+v %v", result, err)
	}
	if err := s.CompleteFormationJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	job = assessmentTestJob(t, s, c.Evidence)
	input, err := s.FormationAssessmentInput(ctx, job)
	if err != nil || len(input.Observations) != 1 {
		t.Fatalf("later input: %+v %v", input, err)
	}
	c.Retention = "durable"
	c.SourceObservationIDs = []int64{input.Observations[0].ID}
	result, err = s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}})
	if err != nil || result.PublishedCount != 1 {
		t.Fatalf("promotion: %+v %v", result, err)
	}
	var n int
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM memory_observation_evidence`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("evidence links %d %v", n, err)
	}
}

func TestAssessmentMergeMovesEvidenceRulesAndReceipts(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "winner")
	c := assessmentTestCandidate()
	c.Retention = "observation"
	job := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	obs, err := s.ListMemoryObservations(ctx, "user", time.Now())
	if err != nil || len(obs) != 1 {
		t.Fatalf("observations %v %v", obs, err)
	}
	c.Retention = "durable"
	c.SourceObservationIDs = []int64{obs[0].ID}
	job = assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := s.MergeUsersTx(ctx, tx, "winner", "user", ""); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	obs, err = s.ListMemoryObservations(ctx, "winner", time.Now())
	if err != nil || len(obs) != 1 {
		t.Fatalf("merged observations %v %v", obs, err)
	}
	for _, table := range []string{"memory_observations", "memory_assessment_inputs", "memory_assessment_receipts"} {
		var n int
		if err := s.sql.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE canonical_user_id='user'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("loser %s %d %v", table, n, err)
		}
	}
	var n int
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM memory_observation_evidence e JOIN memory_entries m ON m.id=e.memory_id JOIN memory_observations o ON o.id=e.observation_id WHERE m.canonical_user_id='winner' AND o.canonical_user_id='winner'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("merged evidence %d %v", n, err)
	}
}

func TestObservationPerTurnAndByteBounds(t *testing.T) {
	for _, kind := range []string{"turn", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			s := newFormationTestStore(t)
			ctx := context.Background()
			for i := 0; i < 100; i++ {
				seedFormationTurn(t, s, "user", "bounds", "evidence")
			}
			tx, err := s.sql.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			c := assessmentTestCandidate()
			c.Retention = "observation"
			c.TTLDays = 30
			if kind == "bytes" {
				c.Statement = strings.Repeat("x", 1000)
				c.Evidence = strings.Repeat("e", 1000)
				c.Context = strings.Repeat("c", 500)
			}
			createdCount := 0
			for i := 0; i < 100; i++ {
				c.ClaimValue = fmt.Sprint(i)
				turn := int64(1)
				if kind == "bytes" {
					turn = int64(i + 1)
				}
				created, err := insertObservationTx(ctx, tx, "user", turn, c, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if created {
					createdCount++
				}
			}
			var live int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM memory_observations`).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if kind == "turn" && createdCount != 5 || kind == "bytes" && (createdCount != 100 || live < 40 || live > 53) {
				t.Fatalf("bound %s: %d", kind, createdCount)
			}
			if kind == "turn" {
				if _, err := tx.Exec(`DELETE FROM memory_observations`); err != nil {
					t.Fatal(err)
				}
				c.ClaimValue = "sixth-after-eviction"
				if created, err := insertObservationTx(ctx, tx, "user", 1, c, time.Now()); err != nil || created {
					t.Fatalf("per-turn eviction bypass: %v %v", created, err)
				}
			}
		})
	}
}

func TestUnsuppressionAdmitsOnlyFreshLegacyEvidenceWithoutOldConfidence(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	output := evaluatedClaimCandidate(t, "I prefer tea.", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, .95, "preference.drink", "tea")
	turn := seedFormationTurn(t, s, "user", "suppression", output.Evidence)
	old := CandidateProposal{Output: output, Source: FormationSource{TurnID: turn}, IdempotencyKey: "suppressed-old"}
	candidate, _, err := s.ProposeCandidate(ctx, "user", old)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.SuppressMemory(ctx, "user", candidate.PublishedMemoryID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if replay, _, err := s.ProposeCandidate(ctx, "user", old); err != nil || replay.PublishedMemoryID != 0 {
		t.Fatalf("suppressed replay: %+v %v", replay, err)
	}
	if ok, err := s.UnsuppressMemory(ctx, "user", id); err != nil || !ok {
		t.Fatalf("unsuppress %v %v", ok, err)
	}
	if replay, _, err := s.ProposeCandidate(ctx, "user", old); err != nil || replay.PublishedMemoryID != 0 {
		t.Fatalf("unsuppressed old replay: %+v %v", replay, err)
	}
	output.Confidence = .6
	freshTurn := seedFormationTurn(t, s, "user", "suppression", output.Evidence)
	fresh, _, err := s.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, Source: FormationSource{TurnID: freshTurn}, IdempotencyKey: "unsuppressed-new"})
	if err != nil || fresh.PublishedMemoryID == 0 {
		t.Fatalf("fresh evidence: %+v %v", fresh, err)
	}
	entry, err := s.EntryByID(fresh.PublishedMemoryID)
	if err != nil || entry.Confidence != .6 {
		t.Fatalf("old confidence leaked: %+v %v", entry, err)
	}
}

func TestAssessmentOlderSourceCannotOverrideNewerPublication(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	old := assessmentTestCandidate()
	oldJob := assessmentTestJob(t, s, old.Evidence)
	if _, err := s.FormationAssessmentInput(ctx, oldJob); err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.ClaimValue = "coffee"
	fresh.Statement = "The user prefers coffee."
	fresh.Evidence = "I prefer coffee."
	fresh.Confidence = .4
	newJob := assessmentTestJob(t, s, fresh.Evidence)
	if result, err := s.ApplyMemoryAssessment(ctx, newJob, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{fresh}}); err != nil || result.PublishedCount != 1 {
		t.Fatalf("new source %+v %v", result, err)
	}
	if result, err := s.ApplyMemoryAssessment(ctx, oldJob, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{old}}); err != nil || result.RejectedCount != 1 || result.PublishedCount != 0 {
		t.Fatalf("old source %+v %v", result, err)
	}
}

func TestAssessmentExpiredObservationReceiptPreventsResurrection(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	c.Retention = "observation"
	job := assessmentTestJob(t, s, c.Evidence)
	batch := AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}
	if _, err := s.ApplyMemoryAssessment(ctx, job, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sql.Exec(`UPDATE memory_observations SET observed_at=?,expires_at=?`, formatTime(time.Now().Add(-8*24*time.Hour)), formatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	counts, err := s.cleanupExpiredSessions(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.ObservationsDeleted != 1 {
		t.Fatalf("cleanup %+v %v", counts, err)
	}
	if result, err := s.ApplyMemoryAssessment(ctx, job, batch); err != nil || result.ObservationCount != 0 {
		t.Fatalf("resurrection %+v %v", result, err)
	}
	obs, err := s.ListMemoryObservations(ctx, "user", time.Now())
	if err != nil || len(obs) != 0 {
		t.Fatalf("resurrected observations %v %v", obs, err)
	}
}

func TestInactiveCandidateReplayAndStaleProfileRepair(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	output := evaluatedClaimCandidate(t, "My name is Ada.", "The user's name is Ada.", policy.CategoryIdentity, policy.ProvenanceUserStatement, policy.SensitivityLow, .95, "identity.name", "ada")
	turn := seedFormationTurn(t, s, "user", "name", output.Evidence)
	proposal := CandidateProposal{Output: output, Source: FormationSource{TurnID: turn}, IdempotencyKey: "inactive-replay"}
	candidate, _, err := s.ProposeCandidate(ctx, "user", proposal)
	if err != nil || candidate.PublishedMemoryID == 0 {
		t.Fatalf("publication %+v %v", candidate, err)
	}
	profile, err := s.ResolveSessionProfile(ctx, "user", "bound", time.Hour)
	if err != nil || !strings.Contains(profile.Content, "Ada") {
		t.Fatalf("bound %+v %v", profile, err)
	}
	if _, err := s.sql.Exec(`UPDATE memory_entries SET status='superseded' WHERE id=?`, candidate.PublishedMemoryID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ProposeCandidate(ctx, "user", proposal); err != nil {
		t.Fatalf("inactive replay: %v", err)
	}
	profile, err = s.ResolveSessionProfile(ctx, "user", "bound", time.Hour)
	if err != nil || strings.Contains(profile.Content, "Ada") {
		t.Fatalf("stale profile %+v %v", profile, err)
	}
	memories, err := s.ListActiveMemories(ctx, "user", time.Now())
	if err != nil || len(memories) != 0 {
		t.Fatalf("inactive resurrection %+v %v", memories, err)
	}
}

func TestAssessmentCommitMonitoringExcludesPayloadAndRollback(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			s := newFormationTestStore(t)
			var logs bytes.Buffer
			s.log = config.NewLogger(level)
			s.log.SetOutput(&logs)
			c := assessmentTestCandidate()
			c.Statement = "private-assessment-canary"
			c.Retention = "observation"
			job := assessmentTestJob(t, s, c.Evidence)
			bad := c
			bad.Evidence = "absent-evidence-canary"
			if _, err := s.ApplyMemoryAssessment(context.Background(), job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c, bad}}); err == nil {
				t.Fatal("invalid batch accepted")
			}
			if strings.Contains(logs.String(), `"event":"memory.assessment.applied"`) {
				t.Fatal("rollback reported as committed")
			}
			batch := AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}
			for i := 0; i < 2; i++ {
				if _, err := s.ApplyMemoryAssessment(context.Background(), job, batch); err != nil {
					t.Fatal(err)
				}
			}
			text := logs.String()
			if strings.Count(text, `"event":"memory.assessment.applied"`) != 1 || !strings.Contains(text, `"observation_count":1`) || strings.Contains(text, "canary") || strings.Contains(text, c.Evidence) {
				t.Fatalf("unsafe or duplicate monitoring: %s", text)
			}
		})
	}
}
