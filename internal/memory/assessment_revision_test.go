package memory

import (
	"context"
	"encoding/json"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"strings"
	"testing"
	"time"
)

func TestLaterAssessmentCanLowerCoherentConfidenceAndRetainsContext(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	first := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, first, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	c.Confidence = .4
	c.Context = "Only on weekdays, not a general preference."
	second := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, second, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	var confidence float64
	var contextText, provenance string
	if err := s.sql.QueryRow(`SELECT confidence,assessment_context,provenance_type FROM memory_entries WHERE status='active'`).Scan(&confidence, &contextText, &provenance); err != nil {
		t.Fatal(err)
	}
	if confidence != .4 || contextText != c.Context || provenance != string(c.Provenance) {
		t.Fatalf("tuple %v %q %q", confidence, contextText, provenance)
	}
	if err := s.CompleteFormationJob(ctx, second, false); err != nil {
		t.Fatal(err)
	}
	c.Provenance = policy.ProvenanceModelInference
	c.EvidenceType = "model_inference"
	c.Confidence = .99
	third := assessmentTestJob(t, s, c.Evidence)
	result, err := s.ApplyMemoryAssessment(ctx, third, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}})
	if err != nil || result.RejectedCount != 1 {
		t.Fatalf("authority downgrade %+v %v", result, err)
	}
	if err := s.CompleteFormationJob(ctx, third, false); err != nil {
		t.Fatal(err)
	}
	c.Provenance = policy.ProvenanceUserStatement
	c.EvidenceType = "direct_statement"
	c.Confidence = .2
	fourth := assessmentTestJob(t, s, c.Evidence)
	result, err = s.ApplyMemoryAssessment(ctx, fourth, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}})
	if err != nil || result.RetiredCount != 1 {
		t.Fatalf("below threshold %+v %v", result, err)
	}
	var retired, reason string
	if err := s.sql.QueryRow(`SELECT retired_at,retirement_reason FROM memory_entries WHERE status='superseded'`).Scan(&retired, &reason); err != nil || reason != "low_confidence" || retired == "" {
		t.Fatalf("historical metadata %q %q %v", retired, reason, err)
	}
}

func TestStaleAssessmentTargetRejectsWithoutRetryAndDefaultDigestIsCanonical(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	job := assessmentTestJob(t, s, c.Evidence)
	batch := AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}
	if _, err := s.ApplyMemoryAssessment(ctx, job, batch); err != nil {
		t.Fatal(err)
	}
	batch.Items[0].Intent = ""
	batch.Items[0].Cardinality = ""
	batch.Items[0].Retention = ""
	if result, err := s.ApplyMemoryAssessment(ctx, job, batch); err != nil || result != (AssessmentResult{}) {
		t.Fatalf("default replay %+v %v", result, err)
	}
	if err := s.CompleteFormationJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	var id, revision int64
	if err := s.sql.QueryRow(`SELECT id,revision FROM memory_entries`).Scan(&id, &revision); err != nil {
		t.Fatal(err)
	}
	c.TargetMemoryID = id
	c.ExpectedRevision = revision
	c.Intent = "retire"
	job = assessmentTestJob(t, s, c.Evidence)
	if _, err := s.FormationAssessmentInput(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sql.Exec(`UPDATE memory_entries SET status='superseded' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}})
	if err != nil || result.RejectedCount != 1 {
		t.Fatalf("stale target %+v %v", result, err)
	}
	if err := s.CompleteFormationJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
}

func TestObservationTimestampAndEvictionReceipt(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	c.Retention = "observation"
	now := time.Now().UTC()
	turn := seedFormationTurn(t, s, "user", "source", c.Evidence)
	source := now.Add(-6 * 24 * time.Hour)
	if _, err := s.sql.Exec(`UPDATE session_turns SET created_at=? WHERE id=?`, formatTime(source), turn); err != nil {
		t.Fatal(err)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if ok, err := insertObservationTx(ctx, tx, "user", turn, c, now); err != nil || !ok {
		t.Fatalf("admit %v %v", ok, err)
	}
	observations, err := listMemoryObservationsTx(ctx, tx, "user", now)
	if err != nil || len(observations) != 1 {
		t.Fatal(err)
	}
	if !observations[0].ObservedAt.Equal(source) || !observations[0].ExpiresAt.Equal(source.Add(7*24*time.Hour)) {
		t.Fatalf("extended timestamp %+v", observations[0])
	}
	if _, err := tx.Exec(`DELETE FROM memory_observations`); err != nil {
		t.Fatal(err)
	}
	if ok, err := insertObservationTx(ctx, tx, "user", turn, c, now); err != nil || ok {
		t.Fatalf("eviction replay %v %v", ok, err)
	}
	c.ClaimValue = "other"
	if ok, err := insertObservationTx(ctx, tx, "user", turn, c, now.Add(2*24*time.Hour)); err != nil || ok {
		t.Fatalf("expired evidence admitted %v %v", ok, err)
	}
}

func TestAssessmentSchemaRejectsWrongFrozenSourceAndOversizedPayload(t *testing.T) {
	s := newFormationTestStore(t)
	job := assessmentTestJob(t, s, "I prefer tea.")
	input := AssessmentInput{Version: 1, Anchor: StoredSessionTurn{ID: job.TurnID, UserID: job.UserID, SessionID: job.SessionID, Generation: job.SessionGeneration}}
	wrong := input
	wrong.Anchor.UserID = "other"
	data, err := json.Marshal(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.sql.Exec(`INSERT INTO memory_assessment_inputs(job_id,canonical_user_id,payload) VALUES(?,?,?)`, job.ID, job.UserID, string(data)); err == nil {
		t.Fatal("cross-tenant frozen input admitted")
	}
	input.Anchor.UserText = strings.Repeat("x", 1048576)
	data, err = json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.sql.Exec(`INSERT INTO memory_assessment_inputs(job_id,canonical_user_id,payload) VALUES(?,?,?)`, job.ID, job.UserID, string(data)); err == nil {
		t.Fatal("oversized frozen input admitted")
	}
	seedAccountUsers(t, s, "other")
	if _, err := s.sql.Exec(`INSERT INTO memory_observation_receipts(canonical_user_id,source_turn_id,digest) VALUES('other',?,?)`, job.TurnID, formationKey("wrong-source")); err == nil {
		t.Fatal("cross-tenant admission receipt accepted")
	}
	if _, err := s.sql.Exec(`INSERT INTO memory_observations(canonical_user_id,source_turn_id,statement,evidence,context,provenance_type,claim_slot,claim_value,observed_at,expires_at) VALUES(?,?,'s','e','','user_statement','notes.fact','v','2020-01-01T00:00:00Z','2020-01-02T00:00:00Z')`, job.UserID, job.TurnID); err == nil {
		t.Fatal("forged observation timestamp accepted")
	}
}

func TestAutomaticReplacementFencesLateLegacyEvidence(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	old := assessmentTestCandidate()
	first := assessmentTestJob(t, s, old.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, first, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{old}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.ClaimValue = "coffee"
	fresh.Statement = "The user prefers coffee."
	fresh.Evidence = "I prefer coffee."
	fresh.Confidence = .99
	second := assessmentTestJob(t, s, fresh.Evidence)
	if result, err := s.ApplyMemoryAssessment(ctx, second, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{fresh}}); err != nil || result.PublishedCount != 1 {
		t.Fatalf("replacement %+v %v", result, err)
	}
	output, err := old.Evaluate(old.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	output.Confidence = 1
	candidate, _, err := s.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, Source: FormationSource{TurnID: first.TurnID}, IdempotencyKey: "late-legacy"})
	if err != nil || candidate.PublishedMemoryID != 0 {
		t.Fatalf("old evidence reactivated %+v %v", candidate, err)
	}
	assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_entries WHERE retirement_reason='replaced' AND retired_at IS NOT NULL`, 1)
}
