package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

func TestPublicationCreatesNewObservationAfterHardDelete(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	request := memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "The user builds Atlas.", Evidence: "I build Atlas.", Confidence: 0.9, Importance: 4}
	first, err := store.publishFixtureMemory(ctx, "user", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HardDeleteMemory(ctx, "user", first.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	request.SourceTurnID = seedFormationTurn(t, store, "user", "fresh-after-delete", request.Evidence)
	second, err := store.publishFixtureMemory(ctx, "user", request)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("hard-deleted memory was unexpectedly reused")
	}
	var observations int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user' AND published_memory_id = ?`, second.ID).Scan(&observations); err != nil || observations != 1 {
		t.Fatalf("fresh observation count=%d err=%v", observations, err)
	}
}

func TestPublicationDoesNotReuseHardDeletedMemory(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	request := memoryFixture{Scope: ScopeLongTerm, Category: "notes", Statement: "The user keeps a private journal.", Evidence: "original evidence"}
	deleted, err := store.publishFixtureMemory(ctx, "user", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HardDeleteMemory(ctx, "user", deleted.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	request.SourceTurnID = seedFormationTurn(t, store, "user", "fresh-after-delete", request.Evidence)
	fresh, err := store.publishFixtureMemory(ctx, "user", request)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == deleted.ID || fresh.Status != StatusActive {
		t.Fatalf("fresh memory=%+v deleted=%+v", fresh, deleted)
	}
	var oldCount int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_entries WHERE id = ?`, deleted.ID).Scan(&oldCount); err != nil || oldCount != 0 {
		t.Fatalf("deleted memory count=%d err=%v", oldCount, err)
	}
}

func TestPublicationDeduplicatesBackgroundFallbackIdentity(t *testing.T) {
	store := newFormationTestStore(t)
	output := evaluatedFormationCandidate(t, "I build Atlas.", "I build Atlas.", "The user builds Atlas.", policy.CategoryProjects)
	background, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, IdempotencyKey: "background-fallback"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := store.publishFixtureMemory(context.Background(), "user", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: output.Statement, Evidence: "additional evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if observation.ID != background.PublishedMemoryID {
		t.Fatalf("reinforced memory=%d background memory=%d", observation.ID, background.PublishedMemoryID)
	}
	var memoryCount, candidateCount int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_entries WHERE canonical_user_id = 'user' AND status = 'active'`).Scan(&memoryCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user' AND published_memory_id = ?`, observation.ID).Scan(&candidateCount); err != nil {
		t.Fatal(err)
	}
	if memoryCount != 1 || candidateCount != 2 {
		t.Fatalf("memory count=%d candidate count=%d", memoryCount, candidateCount)
	}
}

func TestPublicationSupersedesBackgroundFallbackByNormalizedStatement(t *testing.T) {
	store := newFormationTestStore(t)
	oldOutput := evaluatedFormationCandidate(t, "I prefer tea.", "I prefer tea.", "The user prefers tea.", policy.CategoryDurablePreferences)
	oldCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: oldOutput, IdempotencyKey: "background-tea"})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := store.publishFixtureMemory(context.Background(), "user", memoryFixture{
		Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user prefers coffee.",
		Evidence: "I prefer coffee.", Confidence: 1, Supersedes: "  THE user prefers tea  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.EntryByID(oldCandidate.PublishedMemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != "superseded" || replacement.SupersedesID != old.ID {
		t.Fatalf("old=%+v replacement=%+v", old, replacement)
	}
}

func TestPublicationReinforcementKeepsTenantScopedIDAndOutbox(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1", "user-2")
	ctx := context.Background()
	first, err := store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "notes", Statement: "User one fact.", Evidence: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.publishFixtureMemory(ctx, "user-2", memoryFixture{Scope: ScopeLongTerm, Category: "notes", Statement: "User two private fact.", Evidence: "private"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "notes", Statement: "User one fact.", Evidence: "updated without embedding"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != first.ID || updated.UserID != "user-1" || updated.Statement != "User one fact." {
		t.Fatalf("reinforcement returned wrong tenant memory: %+v", updated)
	}
	var secondChanges int
	if err := store.sql.QueryRow(`SELECT count(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind = 'memory' AND entity_id = ?`, second.ID).Scan(&secondChanges); err != nil {
		t.Fatal(err)
	}
	if secondChanges != 1 {
		t.Fatalf("foreign tenant derived change count = %d, want 1", secondChanges)
	}
}

func TestProposeCandidateAtomicallyPublishesApprovedPolicy(t *testing.T) {
	store := newFormationTestStore(t)
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", policy.CategoryProjects)
	candidate, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, IdempotencyKey: "atomic"})
	if err != nil || !created || candidate.State != "approved" || candidate.PublishedMemoryID == 0 {
		t.Fatalf("candidate=%+v created=%v err=%v", candidate, created, err)
	}
	var invalid int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE state = 'approved' AND published_memory_id IS NULL`).Scan(&invalid); err != nil || invalid != 0 {
		t.Fatalf("approved unpublished candidates=%d err=%v", invalid, err)
	}
	replayed, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, IdempotencyKey: "atomic"})
	if err != nil || created || replayed.PublishedMemoryID != candidate.PublishedMemoryID {
		t.Fatalf("replay=%+v created=%v err=%v", replayed, created, err)
	}
}

func TestExplicitRememberCanReplaceEqualStrengthClaim(t *testing.T) {
	store := newFormationTestStore(t)
	oldOutput, err := policy.Evaluate(policy.CandidateInput{
		SourceUserText: "I live in Boston.", Statement: "The user lives in Boston.", Evidence: "I live in Boston.",
		Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect,
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeAgentSave, Scope: policy.ScopeLongTerm,
		Category: policy.CategoryEnvironment, Context: policy.ContextDirectAssertion, Confidence: 1, Importance: 3,
		ClaimSlot: "environment.home_city", ClaimValue: "Boston",
	})
	if err != nil {
		t.Fatal(err)
	}
	oldCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: oldOutput, IdempotencyKey: "old-city"})
	if err != nil {
		t.Fatal(err)
	}
	newOutput, err := policy.Evaluate(policy.CandidateInput{
		SourceUserText: "Remember that I live in Porto.", Statement: "The user lives in Porto.", Evidence: "I live in Porto.",
		Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect,
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeExplicitRemember, Scope: policy.ScopeLongTerm,
		Category: policy.CategoryEnvironment, Context: policy.ContextDirectAssertion, Confidence: 1, Importance: 3,
		ClaimSlot: "environment.home_city", ClaimValue: "Porto",
	})
	if err != nil {
		t.Fatal(err)
	}
	newCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: newOutput, IdempotencyKey: "new-city"})
	if err != nil {
		t.Fatal(err)
	}
	if newCandidate.PublishedMemoryID == 0 || newCandidate.SupersedesMemoryID != oldCandidate.PublishedMemoryID {
		t.Fatalf("old=%+v new=%+v", oldCandidate, newCandidate)
	}
}

func TestProposeCandidateRollsBackEveryPublicationStage(t *testing.T) {
	for _, stage := range []string{"validated", "canonical_written", "vector_written", "supersession_written", "profile_written", "candidate_published"} {
		t.Run(stage, func(t *testing.T) {
			store := newFormationTestStore(t)
			store.formationFailpoint = func(current string) error {
				if current == stage {
					return errors.New("injected")
				}
				return nil
			}
			output := evaluatedFormationCandidate(t, "I moved to Porto", "I moved to Porto", "The user lives in Porto.", policy.CategoryEnvironment)
			if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, IdempotencyKey: stage}); err == nil {
				t.Fatal("expected transaction failure")
			}
			for table, want := range map[string]int{"memory_candidates": 0, "memory_entries": 0} {
				var count int
				if err := store.sql.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != want {
					t.Fatalf("%s count=%d err=%v", table, count, err)
				}
			}
		})
	}
}

func TestProposeCandidateDuplicateReinforcementIsIdempotent(t *testing.T) {
	store := newFormationTestStore(t)
	firstOutput := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.6, "preference.drink", "tea")
	secondOutput := firstOutput
	secondOutput.Confidence = 0.5
	secondOutput.Statement = "The user's preferred drink is tea."
	secondOutput.Evidence = "My preferred drink is tea"
	first, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: firstOutput, IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: secondOutput, IdempotencyKey: "second"})
	if err != nil || second.PublishedMemoryID != first.PublishedMemoryID {
		t.Fatalf("second=%+v first=%+v err=%v", second, first, err)
	}
	memory, err := store.EntryByID(first.PublishedMemoryID)
	if err != nil || memory.Confidence != 0.6 || memory.EvidenceCount != 2 || memory.Statement != firstOutput.Statement {
		t.Fatalf("reinforced memory=%+v err=%v", memory, err)
	}
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: secondOutput, IdempotencyKey: "second"}); err != nil {
		t.Fatal(err)
	}
	again, _ := store.EntryByID(first.PublishedMemoryID)
	if again.Confidence != memory.Confidence || again.EvidenceCount != 2 {
		t.Fatalf("idempotent replay changed memory: before=%+v after=%+v", memory, again)
	}
}

func TestProposeCandidateConsolidatesLowThenHighEvidence(t *testing.T) {
	store := newFormationTestStore(t)
	low := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.2, "preference.drink", "tea")
	lowTurn := seedFormationTurn(t, store, "user", "consolidation", low.Evidence)
	first, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: low, IdempotencyKey: "low-then-high-low", Source: FormationSource{TurnID: lowTurn}})
	if err != nil || first.State != "proposed" || first.PublishedMemoryID != 0 {
		t.Fatalf("low candidate=%+v err=%v", first, err)
	}
	high := evaluatedClaimCandidate(t, "My preferred drink is tea", "The user's preferred drink is tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.8, "preference.drink", "tea")
	highTurn := seedFormationTurn(t, store, "user", "consolidation", high.Evidence)
	second, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: high, IdempotencyKey: "low-then-high-high", Source: FormationSource{TurnID: highTurn}})
	if err != nil || second.PublishedMemoryID == 0 {
		t.Fatalf("high candidate=%+v err=%v", second, err)
	}
	first, err = store.loadFixtureCandidate(context.Background(), "user", first.ID)
	memory, memoryErr := store.EntryByID(second.PublishedMemoryID)
	if err != nil || memoryErr != nil || first.State != "approved" || first.PublishedMemoryID != second.PublishedMemoryID || first.Confidence != 0.2 || memory.Confidence != 0.8 || memory.EvidenceCount != 2 {
		t.Fatalf("low=%+v memory=%+v load_err=%v memory_err=%v", first, memory, err, memoryErr)
	}
}

func TestProposeCandidateConsolidatesHighThenLowWithoutCanonicalDecrease(t *testing.T) {
	store := newFormationTestStore(t)
	high := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.8, "preference.drink", "tea")
	highTurn := seedFormationTurn(t, store, "user", "consolidation", high.Evidence)
	first, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: high, IdempotencyKey: "high-then-low-high", Source: FormationSource{TurnID: highTurn}})
	if err != nil {
		t.Fatal(err)
	}
	low := evaluatedClaimCandidate(t, "I like tea", "The user likes tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.2, "preference.drink", "tea")
	lowTurn := seedFormationTurn(t, store, "user", "consolidation", low.Evidence)
	second, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: low, IdempotencyKey: "high-then-low-low", Source: FormationSource{TurnID: lowTurn}})
	if err != nil || second.State != "approved" || second.PublishedMemoryID != first.PublishedMemoryID || second.Confidence != 0.2 || second.DecisionReason != "observation supports an already-active claim" {
		t.Fatalf("low candidate=%+v err=%v", second, err)
	}
	memory, err := store.EntryByID(first.PublishedMemoryID)
	if err != nil || memory.Confidence != 0.8 || memory.Statement != high.Statement || memory.EvidenceCount != 2 {
		t.Fatalf("memory=%+v err=%v", memory, err)
	}
}

func TestProposeCandidateCanonicalMetadataUsesStrongestEvidenceAuthority(t *testing.T) {
	store := newFormationTestStore(t)
	direct := evaluatedClaimCandidate(t, "I use Arch Linux", "The user uses Arch Linux.", policy.CategoryEnvironment, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.2, "environment.linux_distribution", "arch_linux")
	direct.ClaimValue = "arch_family"
	directTurn := seedFormationTurn(t, store, "user", "metadata", direct.Evidence)
	directCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: direct, IdempotencyKey: "metadata-direct", Source: FormationSource{TurnID: directTurn}})
	if err != nil || directCandidate.State != "proposed" {
		t.Fatalf("direct candidate=%+v err=%v", directCandidate, err)
	}
	inferred := evaluatedClaimCandidate(t, "Considering pacman packages for file management.", "The user may use or be evaluating a pacman-based Arch-family Linux environment.", policy.CategoryEnvironment, policy.ProvenanceModelInference, policy.SensitivityLow, 0.8, "environment.linux_distribution", "arch_family")
	inferredTurn := seedFormationTurn(t, store, "user", "metadata", inferred.Evidence)
	candidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: inferred, IdempotencyKey: "metadata-inferred", Source: FormationSource{TurnID: inferredTurn}})
	if err != nil || candidate.PublishedMemoryID == 0 {
		t.Fatalf("inferred candidate=%+v err=%v", candidate, err)
	}
	memory, err := store.EntryByID(candidate.PublishedMemoryID)
	if err != nil || memory.Confidence != 0.2 || memory.ProvenanceType != "user_statement" || memory.Statement != direct.Statement || memory.EvidenceCount != 2 {
		t.Fatalf("memory=%+v err=%v", memory, err)
	}
}

func TestProposeCandidateOnlyConsolidatesEligibleExactClaimEvidence(t *testing.T) {
	store := newFormationTestStore(t)
	seedAccountUsers(t, store, "other")
	base := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.2, "preference.drink", "tea")
	type staged struct {
		userID string
		key    string
		output policy.CandidateOutput
		turnID int64
	}
	undeliveredTurn := seedFormationTurn(t, store, "user", "filters", base.Evidence)
	failedTurn := seedFormationTurn(t, store, "user", "filters", base.Evidence)
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?; UPDATE session_turns SET delivery_failed_at = ? WHERE id = ?`, undeliveredTurn, formatTime(time.Now().UTC()), failedTurn); err != nil {
		t.Fatal(err)
	}
	differentValue := base
	differentValue.ClaimValue = "coffee"
	differentScope := base
	differentScope.Scope = policy.ScopeShortTerm
	cases := []staged{
		{userID: "user", key: "filter-undelivered", output: base, turnID: undeliveredTurn},
		{userID: "user", key: "filter-failed", output: base, turnID: failedTurn},
		{userID: "user", key: "filter-value", output: differentValue, turnID: seedFormationTurn(t, store, "user", "filters", differentValue.Evidence)},
		{userID: "user", key: "filter-scope", output: differentScope, turnID: seedFormationTurn(t, store, "user", "filters", differentScope.Evidence)},
		{userID: "other", key: "filter-tenant", output: base, turnID: seedFormationTurn(t, store, "other", "filters", base.Evidence)},
	}
	var candidates []FormationCandidate
	for _, item := range cases {
		candidate, _, err := store.ProposeCandidate(context.Background(), item.userID, CandidateProposal{Output: item.output, IdempotencyKey: item.key, Source: FormationSource{TurnID: item.turnID}})
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	high := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.8, "preference.drink", "tea")
	highTurn := seedFormationTurn(t, store, "user", "filters", high.Evidence)
	active, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: high, IdempotencyKey: "filter-active", Source: FormationSource{TurnID: highTurn}})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := store.EntryByID(active.PublishedMemoryID)
	if err != nil || memory.EvidenceCount != 1 {
		t.Fatalf("memory=%+v err=%v", memory, err)
	}
	for index, candidate := range candidates {
		loaded, err := store.loadFixtureCandidate(context.Background(), cases[index].userID, candidate.ID)
		if err != nil || loaded.PublishedMemoryID != 0 || loaded.State != "proposed" {
			t.Fatalf("case=%s candidate=%+v err=%v", cases[index].key, loaded, err)
		}
	}
}

func TestProposeCandidateConsolidationReplayKeepsEvidenceCountStable(t *testing.T) {
	store := newFormationTestStore(t)
	low := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.2, "preference.drink", "tea")
	lowTurn := seedFormationTurn(t, store, "user", "replay", low.Evidence)
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: low, IdempotencyKey: "replay-low", Source: FormationSource{TurnID: lowTurn}}); err != nil {
		t.Fatal(err)
	}
	high := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.8, "preference.drink", "tea")
	highTurn := seedFormationTurn(t, store, "user", "replay", high.Evidence)
	proposal := CandidateProposal{Output: high, IdempotencyKey: "replay-high", Source: FormationSource{TurnID: highTurn}}
	active, _, err := store.ProposeCandidate(context.Background(), "user", proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.ProposeCandidate(context.Background(), "user", proposal); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	memory, err := store.EntryByID(active.PublishedMemoryID)
	if err != nil || memory.EvidenceCount != 2 || memory.Confidence != 0.8 {
		t.Fatalf("memory=%+v err=%v", memory, err)
	}
}

func TestProposeCandidateTreatsReinforcementTargetAsValidatedHint(t *testing.T) {
	store := newFormationTestStore(t)
	seedAccountUsers(t, store, "other")
	base := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.6, "preference.drink", "tea")
	baseCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: base, IdempotencyKey: "target-base", Source: FormationSource{TurnID: seedFormationTurn(t, store, "user", "target", "I prefer tea")}})
	if err != nil {
		t.Fatal(err)
	}
	wrong := evaluatedClaimCandidate(t, "I use Go", "The user uses Go.", policy.CategoryProjects, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.9, "project.language", "go")
	wrongCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: wrong, IdempotencyKey: "wrong-target-memory"})
	if err != nil {
		t.Fatal(err)
	}
	foreignCandidate, _, err := store.ProposeCandidate(context.Background(), "other", CandidateProposal{Output: base, IdempotencyKey: "foreign-target-memory"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		targetID   int64
		confidence float64
	}{
		{name: "valid", targetID: baseCandidate.PublishedMemoryID, confidence: 0.7},
		{name: "wrong identity", targetID: wrongCandidate.PublishedMemoryID, confidence: 0.8},
		{name: "cross tenant", targetID: foreignCandidate.PublishedMemoryID, confidence: 0.9},
		{name: "missing", targetID: foreignCandidate.PublishedMemoryID + 1000, confidence: 0.95},
	}
	var lastTurn int64
	for index, test := range tests {
		incoming := base
		incoming.Confidence = test.confidence
		incoming.Statement = "The user's preferred beverage is tea."
		incoming.Evidence = "My preferred beverage is tea"
		turnID := seedFormationTurn(t, store, "user", "target", incoming.Evidence)
		lastTurn = turnID
		candidate, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: incoming, IdempotencyKey: fmt.Sprintf("target-%d", index), TargetMemoryID: test.targetID, Source: FormationSource{TurnID: turnID}})
		if err != nil || !created || candidate.PublishedMemoryID != baseCandidate.PublishedMemoryID {
			t.Fatalf("%s candidate=%+v created=%v err=%v", test.name, candidate, created, err)
		}
	}
	memory, err := store.EntryByID(baseCandidate.PublishedMemoryID)
	if err != nil || memory.Confidence != 0.95 || memory.Statement != "The user's preferred beverage is tea." || memory.EvidenceCount != 5 {
		t.Fatalf("reinforced memory=%+v err=%v", memory, err)
	}
	wrongMemory, _ := store.EntryByID(wrongCandidate.PublishedMemoryID)
	foreignMemory, _ := store.EntryByID(foreignCandidate.PublishedMemoryID)
	if wrongMemory.EvidenceCount != 1 || foreignMemory.EvidenceCount != 1 {
		t.Fatalf("target redirected evidence: wrong=%+v foreign=%+v", wrongMemory, foreignMemory)
	}
	before := memory
	replayedOutput := base
	replayedOutput.Confidence = 0.95
	replayedOutput.Statement = "The user's preferred beverage is tea."
	replayedOutput.Evidence = "My preferred beverage is tea"
	if _, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: replayedOutput, IdempotencyKey: "target-3", TargetMemoryID: wrongCandidate.PublishedMemoryID, Source: FormationSource{TurnID: lastTurn}}); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	after, _ := store.EntryByID(baseCandidate.PublishedMemoryID)
	if after.Confidence != before.Confidence || after.EvidenceCount != before.EvidenceCount || after.Statement != before.Statement {
		t.Fatalf("replay changed canonical memory: before=%+v after=%+v", before, after)
	}
}

func TestProposeCandidateBlocksWeakConflictAndSupersedesWithStrongerEvidence(t *testing.T) {
	store := newFormationTestStore(t)
	tea := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.8, "preference.drink", "tea")
	active, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: tea, IdempotencyKey: "tea"})
	if err != nil {
		t.Fatal(err)
	}
	coffee := evaluatedClaimCandidate(t, "I prefer coffee", "The user prefers coffee.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.7, "preference.drink", "coffee")
	blocked, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: coffee, IdempotencyKey: "weak-coffee"})
	if err != nil || blocked.State != "approved" || blocked.PublishedMemoryID != 0 {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
	replayed, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: coffee, IdempotencyKey: "weak-coffee"})
	if err != nil || created || replayed.PublishedMemoryID != 0 {
		t.Fatalf("blocked replay=%+v created=%v err=%v", replayed, created, err)
	}
	coffee = evaluatedClaimCandidate(t, "I prefer coffee", "The user prefers coffee.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.95, "preference.drink", "coffee")
	replacement, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: coffee, IdempotencyKey: "strong-coffee"})
	if err != nil || replacement.PublishedMemoryID == 0 || replacement.PublishedMemoryID == active.PublishedMemoryID {
		t.Fatalf("replacement=%+v active=%+v err=%v", replacement, active, err)
	}
	old, _ := store.EntryByID(active.PublishedMemoryID)
	if old.Status != "superseded" {
		t.Fatalf("old memory=%+v", old)
	}
}

func TestFactSlotObservationsRemainDistinctByValue(t *testing.T) {
	store := newFormationTestStore(t)
	first := evaluatedClaimCandidate(t, "I enjoy tea", "The user enjoys tea.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.8, "durable_preferences.fact", "enjoys_tea")
	second := evaluatedClaimCandidate(t, "I enjoy hiking", "The user enjoys hiking.", policy.CategoryDurablePreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.8, "durable_preferences.fact", "enjoys_hiking")
	firstCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: first, IdempotencyKey: "fact-tea"})
	if err != nil {
		t.Fatal(err)
	}
	secondCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: second, IdempotencyKey: "fact-hiking"})
	if err != nil {
		t.Fatal(err)
	}
	if firstCandidate.PublishedMemoryID == secondCandidate.PublishedMemoryID {
		t.Fatalf("distinct fact values shared memory %d", firstCandidate.PublishedMemoryID)
	}
	memories, err := store.ListMemories("user", ScopeLongTerm, "durable_preferences", 10)
	if err != nil || len(memories) != 2 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestFallbackFactSupersessionMatchesNormalizedStatement(t *testing.T) {
	store := newFormationTestStore(t)
	tea := evaluatedFormationCandidate(t, "I prefer tea.", "I prefer tea.", "The user prefers tea.", policy.CategoryDurablePreferences)
	active, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: tea, IdempotencyKey: "fallback-tea"})
	if err != nil {
		t.Fatal(err)
	}
	coffee := evaluatedFormationCandidate(t, "I prefer coffee.", "I prefer coffee.", "The user prefers coffee.", policy.CategoryDurablePreferences)
	coffee.Confidence = 0.95
	replacement, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{
		Output: coffee, IdempotencyKey: "fallback-coffee", SupersedesStatement: "  THE USER prefers tea  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.EntryByID(active.PublishedMemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.PublishedMemoryID == 0 || replacement.SupersedesMemoryID != old.ID || old.Status != "superseded" {
		t.Fatalf("replacement=%+v old=%+v", replacement, old)
	}
}

func TestDirectEvidenceUpgradesInferenceAndInferenceCannotReplaceDirect(t *testing.T) {
	store := newFormationTestStore(t)
	inferred := evaluatedClaimCandidate(t, "Considering pacman packages for file management.", "The user may use or be evaluating a pacman-based Arch-family Linux environment.", policy.CategoryEnvironment, policy.ProvenanceModelInference, policy.SensitivityLow, 0.9, "environment.linux_distribution", "arch_family")
	inferredMemory, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: inferred, IdempotencyKey: "inferred-arch"})
	if err != nil || inferredMemory.PublishedMemoryID == 0 {
		t.Fatalf("inferred=%+v err=%v", inferredMemory, err)
	}
	direct := inferred
	direct.Statement = "The user uses Arch Linux."
	direct.Evidence = "I use Arch Linux."
	direct.Provenance = policy.ProvenanceUserStatement
	direct.SourceAuthority = policy.AuthorityUserDirect
	direct.Confidence = 0.6
	directMemory, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: direct, IdempotencyKey: "direct-arch"})
	if err != nil || directMemory.PublishedMemoryID != inferredMemory.PublishedMemoryID {
		t.Fatalf("direct=%+v inferred=%+v err=%v", directMemory, inferredMemory, err)
	}
	upgraded, err := store.EntryByID(inferredMemory.PublishedMemoryID)
	if err != nil || upgraded.ProvenanceType != "user_statement" || upgraded.SourceAuthority != "user_direct" {
		t.Fatalf("upgraded=%+v err=%v", upgraded, err)
	}
	inferredConflict := inferred
	inferredConflict.Statement = "The user may use Fedora Linux."
	inferredConflict.Evidence = "Considering dnf packages for file management."
	inferredConflict.ClaimValue = "fedora_linux"
	inferredConflict.Confidence = 1
	blocked, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: inferredConflict, IdempotencyKey: "inferred-fedora"})
	if err != nil || blocked.State != "approved" || blocked.PublishedMemoryID != 0 {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
}

func TestSameTurnPolicyPromotionPublishesInReconciliationTransaction(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go")
	low := evaluatedClaimCandidate(t, "I use Go", "The user uses Go.", policy.CategoryProjects, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.2, "project.language", "go")
	first, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: low, IdempotencyKey: "low", Source: FormationSource{TurnID: turnID}})
	if err != nil || first.State != "proposed" || first.PublishedMemoryID != 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	high := low
	high.Approval = policy.ApprovalApproved
	high.Confidence = 0.9
	merged, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: high, IdempotencyKey: "high", Source: FormationSource{TurnID: turnID}})
	if err != nil || created || merged.ID != first.ID || merged.State != "approved" || merged.PublishedMemoryID == 0 {
		t.Fatalf("merged=%+v created=%v err=%v", merged, created, err)
	}
}
