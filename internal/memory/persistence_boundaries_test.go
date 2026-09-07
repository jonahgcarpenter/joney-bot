package memory

import (
	"context"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

func TestCorrectionUsesExactTargetDespiteLegacyStatementCollision(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	c.Category = policy.CategoryProjects
	c.ClaimSlot = "project.language"
	c.Cardinality = "multiple"
	c.ClaimValue = "c++"
	c.Statement = "The user uses C++."
	c.Evidence = "I use C++ and C#."
	other := c
	other.ClaimValue = "c#"
	other.Statement = "The user uses C#."
	first := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, first, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c, other}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	var target, revision int64
	if err := s.sql.QueryRow(`SELECT id,revision FROM memory_entries WHERE claim_value='c#'`).Scan(&target, &revision); err != nil {
		t.Fatal(err)
	}
	correction := other
	correction.ClaimValue = "rust"
	correction.Statement = "The user uses Rust."
	correction.Evidence = "I switched from C# to Rust."
	correction.Intent = "correction"
	correction.TargetMemoryID = target
	correction.ExpectedRevision = revision
	next := assessmentTestJob(t, s, correction.Evidence)
	if result, err := s.ApplyMemoryAssessment(ctx, next, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{correction}}); err != nil || result.PublishedCount != 1 {
		t.Fatalf("correction %+v %v", result, err)
	}
	assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_entries WHERE claim_value='c++' AND status='active'`, 1)
	assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_entries WHERE id=? AND retirement_reason='replaced'`, 1, target)
}

func TestSuppressionMatchesSeparatorsButPreservesPunctuation(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	output := evaluatedClaimCandidate(t, "I live in New York.", "The user lives in New York.", policy.CategoryIdentity, policy.ProvenanceUserStatement, policy.SensitivityLow, .9, "identity.location", "new_york")
	output.ClaimValue = "new_york"
	source := seedFormationTurn(t, s, "user", "legacy-location", output.Evidence)
	original, _, err := s.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, Source: FormationSource{TurnID: source}})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.SuppressMemory(ctx, "user", original.PublishedMemoryID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	output.ClaimValue = "new york"
	if candidate, _, err := s.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, Source: FormationSource{TurnID: source}}); err != nil || candidate.PublishedMemoryID != 0 {
		t.Fatalf("separator suppression %+v %v", candidate, err)
	}
	if _, err := s.UnsuppressMemory(ctx, "user", rule); err != nil {
		t.Fatal(err)
	}
	if candidate, _, err := s.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, Source: FormationSource{TurnID: source}}); err != nil || candidate.PublishedMemoryID != 0 {
		t.Fatalf("separator cutoff %+v %v", candidate, err)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO memory_suppressions(canonical_user_id,statement,claim_slot,claim_value,through_turn_id,created_at) VALUES('user','C++','project.language','c++',999,?)`, formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if blocked, err := suppressedClaimTx(ctx, tx, "user", "project.language", "c#", source); err != nil || blocked {
		t.Fatalf("punctuation collapsed: %v %v", blocked, err)
	}
}

func TestRetainedPublicationCannotReplaceNewerDifferentValue(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	oldSource := seedFormationTurn(t, s, "user", "old", "I prefer tea.")
	c := assessmentTestCandidate()
	c.ClaimValue = "coffee"
	c.Statement = "The user prefers coffee."
	c.Evidence = "I prefer coffee."
	c.Confidence = .4
	job := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	old := evaluatedClaimCandidate(t, "I prefer tea.", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 1, "preference.drink", "tea")
	candidate, _, err := s.ProposeCandidate(ctx, "user", CandidateProposal{Output: old, Source: FormationSource{TurnID: oldSource, ExtractorVersion: AgentSaveExtractorVersion}})
	if err != nil || candidate.PublishedMemoryID != 0 {
		t.Fatalf("stale retained proposal %+v %v", candidate, err)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Retained pattern aggregation also reaches the shared publication boundary directly.
	id, err := s.publishCandidateTx(ctx, tx, FormationCandidate{UserID: "user", Scope: string(old.Scope), ClaimSlot: old.ClaimSlot, ClaimValue: old.ClaimValue, SourceTurnID: oldSource, State: "approved", Confidence: 1, Provenance: string(old.Provenance)}, 0)
	if err != nil || id != 0 {
		t.Fatalf("stale direct publication %d %v", id, err)
	}
}

func TestUnchangedAssessmentDoesNotRefreshFrozenProfile(t *testing.T) {
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
	frozen, err := s.ResolveSessionProfile(ctx, "user", "frozen", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Category: "identity", Statement: "The user's name is Ada.", Confidence: .99}); err != nil {
		t.Fatal(err)
	}
	next := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, next, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	after, err := s.ResolveSessionProfile(ctx, "user", "frozen", time.Hour)
	if err != nil || after.Content != frozen.Content || after.Version != frozen.Version {
		t.Fatalf("unchanged reinforcement rebound profile: before=%+v after=%+v err=%v", frozen, after, err)
	}
}

func TestMergeReconcilesInactiveCutoffAndPreservesProtectedAssessment(t *testing.T) {
	for _, remove := range []bool{true, false} {
		t.Run(map[bool]string{true: "cutoff", false: "newer-assessment"}[remove], func(t *testing.T) {
			ctx := context.Background()
			s := newFormationTestStore(t)
			seedAccountUsers(t, s, "winner")
			c := assessmentTestCandidate()
			job := assessmentTestJob(t, s, c.Evidence)
			if _, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
				t.Fatal(err)
			}
			through := job.TurnID
			if !remove {
				through--
			}
			if _, err := s.sql.Exec(`INSERT INTO memory_suppressions(canonical_user_id,statement,claim_slot,claim_value,is_active,through_turn_id,created_at) VALUES('winner','','preference.drink','tea',0,?,?)`, through, formatTime(time.Now())); err != nil {
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
			want := 1
			if remove {
				want = 0
			}
			assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_entries WHERE canonical_user_id='winner'`, want)
		})
	}
}

func TestReceiptMaintenanceIsSourceAwareAndReportsCommittedCounts(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	c.Retention = "observation"
	job := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, job, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	counts, err := s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.AssessmentReceiptsDeleted != 0 || counts.ObservationReceiptsDeleted != 0 {
		t.Fatalf("live source receipts %+v %v", counts, err)
	}
	if _, err := s.ResetSession(ctx, "user", "assessment", time.Hour); err != nil {
		t.Fatal(err)
	}
	counts, err = s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.AssessmentReceiptsDeleted != 0 {
		t.Fatalf("live observation receipt %+v %v", counts, err)
	}
	if _, err := s.sql.Exec(`DELETE FROM memory_observations; CREATE TRIGGER fail_receipt_cleanup BEFORE DELETE ON memory_observation_receipts BEGIN SELECT RAISE(ABORT,'test rollback'); END;`); err != nil {
		t.Fatal(err)
	}
	counts, err = s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err == nil || counts.AssessmentReceiptsDeleted != 0 || counts.ObservationReceiptsDeleted != 0 {
		t.Fatalf("uncommitted receipt counts %+v %v", counts, err)
	}
	assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_assessment_receipts`, 1)
	if _, err := s.sql.Exec(`DROP TRIGGER fail_receipt_cleanup`); err != nil {
		t.Fatal(err)
	}
	counts, err = s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.AssessmentReceiptsDeleted != 1 || counts.ObservationReceiptsDeleted != 1 || counts.Changed() < 2 {
		t.Fatalf("receipt cleanup %+v %v", counts, err)
	}
}

func TestReceiptCleanupRetainsLiveDependentJobButNotEmptyAbsentSource(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	c := assessmentTestCandidate()
	c.Retention = "observation"
	source := seedFormationTurn(t, s, "user", "origin", c.Evidence)
	if _, _, err := s.EnqueueAssessmentFormationJob(ctx, FormationSource{TurnID: source}, "user"); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimFormationJob(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyMemoryAssessment(ctx, first, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	dependent := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.FormationAssessmentInput(ctx, dependent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetSession(ctx, "user", "origin", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sql.Exec(`DELETE FROM memory_observations`); err != nil {
		t.Fatal(err)
	}
	counts, err := s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.AssessmentReceiptsDeleted != 0 || counts.ObservationReceiptsDeleted != 0 {
		t.Fatalf("live dependency lost receipt %+v %v", counts, err)
	}
	if _, err := s.ApplyMemoryAssessment(ctx, dependent, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, dependent, false); err != nil {
		t.Fatal(err)
	}
	counts, err = s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.AssessmentReceiptsDeleted != 1 || counts.ObservationReceiptsDeleted != 1 {
		t.Fatalf("terminal dependency retained receipt %+v %v", counts, err)
	}
	assertStoreCount(t, s.sql, `SELECT COUNT(*) FROM memory_assessment_receipts`, 1)
	if _, err := s.ResetSession(ctx, "user", "assessment", time.Hour); err != nil {
		t.Fatal(err)
	}
	counts, err = s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.AssessmentReceiptsDeleted != 1 {
		t.Fatalf("empty receipt not collected %+v %v", counts, err)
	}
}

func TestMergeKeepsCoherentNewerTupleInsteadOfSyntheticSourceMaximum(t *testing.T) {
	ctx := context.Background()
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "winner")
	c := assessmentTestCandidate()
	source := seedFormationTurn(t, s, "winner", "older", c.Evidence)
	if _, _, err := s.EnqueueAssessmentFormationJob(ctx, FormationSource{TurnID: source}, "winner"); err != nil {
		t.Fatal(err)
	}
	older, err := s.ClaimFormationJob(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyMemoryAssessment(ctx, older, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, older, false); err != nil {
		t.Fatal(err)
	}
	c.Confidence = .4
	c.Context = "Only on weekends."
	newer := assessmentTestJob(t, s, c.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, newer, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{c}}); err != nil {
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
	var confidence float64
	var sourceID int64
	var temporal string
	if err := s.sql.QueryRow(`SELECT confidence,assessed_source_turn_id,assessment_context FROM memory_entries WHERE canonical_user_id='winner' AND status='active'`).Scan(&confidence, &sourceID, &temporal); err != nil || confidence != .4 || sourceID != newer.TurnID || temporal != c.Context {
		t.Fatalf("merged tuple %v %d %q %v", confidence, sourceID, temporal, err)
	}
}
