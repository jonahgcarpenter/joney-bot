package usermemory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

func TestProposeCandidateAtomicallyPublishesApprovedPolicy(t *testing.T) {
	store := newFormationTestStore(t)
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", memoryformation.CategoryProjects)
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
	oldOutput, err := memoryformation.Evaluate(memoryformation.CandidateInput{
		SourceUserText: "I live in Boston.", Statement: "The user lives in Boston.", Evidence: "I live in Boston.",
		Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect,
		Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModeAgentSave, Scope: memoryformation.ScopeLongTerm,
		Category: memoryformation.CategoryEnvironment, Context: memoryformation.ContextDirectAssertion, Confidence: 1, Importance: 3,
		ClaimSlot: "environment.home_city", ClaimValue: "Boston",
	})
	if err != nil {
		t.Fatal(err)
	}
	oldCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: oldOutput, IdempotencyKey: "old-city"})
	if err != nil {
		t.Fatal(err)
	}
	newOutput, err := memoryformation.Evaluate(memoryformation.CandidateInput{
		SourceUserText: "Remember that I live in Porto.", Statement: "The user lives in Porto.", Evidence: "I live in Porto.",
		Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect,
		Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModeExplicitRemember, Scope: memoryformation.ScopeLongTerm,
		Category: memoryformation.CategoryEnvironment, Context: memoryformation.ContextDirectAssertion, Confidence: 1, Importance: 3,
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
			output := evaluatedFormationCandidate(t, "I moved to Porto", "I moved to Porto", "The user lives in Porto.", memoryformation.CategoryEnvironment)
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
	firstOutput := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.6, "preference.drink", "tea")
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
	low := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.2, "preference.drink", "tea")
	lowTurn := seedFormationTurn(t, store, "user", "consolidation", low.Evidence)
	first, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: low, IdempotencyKey: "low-then-high-low", Source: FormationSource{TurnID: lowTurn}})
	if err != nil || first.State != "proposed" || first.PublishedMemoryID != 0 {
		t.Fatalf("low candidate=%+v err=%v", first, err)
	}
	high := evaluatedClaimCandidate(t, "My preferred drink is tea", "The user's preferred drink is tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.8, "preference.drink", "tea")
	highTurn := seedFormationTurn(t, store, "user", "consolidation", high.Evidence)
	second, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: high, IdempotencyKey: "low-then-high-high", Source: FormationSource{TurnID: highTurn}})
	if err != nil || second.PublishedMemoryID == 0 {
		t.Fatalf("high candidate=%+v err=%v", second, err)
	}
	first, err = store.LoadCandidate(context.Background(), "user", first.ID)
	memory, memoryErr := store.EntryByID(second.PublishedMemoryID)
	if err != nil || memoryErr != nil || first.State != "approved" || first.PublishedMemoryID != second.PublishedMemoryID || first.Confidence != 0.2 || memory.Confidence != 0.8 || memory.EvidenceCount != 2 {
		t.Fatalf("low=%+v memory=%+v load_err=%v memory_err=%v", first, memory, err, memoryErr)
	}
}

func TestProposeCandidateConsolidatesHighThenLowWithoutCanonicalDecrease(t *testing.T) {
	store := newFormationTestStore(t)
	high := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.8, "preference.drink", "tea")
	highTurn := seedFormationTurn(t, store, "user", "consolidation", high.Evidence)
	first, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: high, IdempotencyKey: "high-then-low-high", Source: FormationSource{TurnID: highTurn}})
	if err != nil {
		t.Fatal(err)
	}
	low := evaluatedClaimCandidate(t, "I like tea", "The user likes tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.2, "preference.drink", "tea")
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
	direct := evaluatedClaimCandidate(t, "I use Arch Linux", "The user uses Arch Linux.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.2, "environment.linux_distribution", "arch_linux")
	direct.ClaimValue = "arch_family"
	directTurn := seedFormationTurn(t, store, "user", "metadata", direct.Evidence)
	directCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: direct, IdempotencyKey: "metadata-direct", Source: FormationSource{TurnID: directTurn}})
	if err != nil || directCandidate.State != "proposed" {
		t.Fatalf("direct candidate=%+v err=%v", directCandidate, err)
	}
	inferred := evaluatedClaimCandidate(t, "Considering pacman packages for file management.", "The user may use or be evaluating a pacman-based Arch-family Linux environment.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceModelInference, memoryformation.SensitivityLow, 0.8, "environment.linux_distribution", "arch_family")
	inferredTurn := seedFormationTurn(t, store, "user", "metadata", inferred.Evidence)
	candidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: inferred, IdempotencyKey: "metadata-inferred", Source: FormationSource{TurnID: inferredTurn}})
	if err != nil || candidate.PublishedMemoryID == 0 {
		t.Fatalf("inferred candidate=%+v err=%v", candidate, err)
	}
	memory, err := store.EntryByID(candidate.PublishedMemoryID)
	if err != nil || memory.Confidence != 0.8 || memory.ProvenanceType != "user_statement" || memory.Statement != direct.Statement || memory.EvidenceCount != 2 {
		t.Fatalf("memory=%+v err=%v", memory, err)
	}
}

func TestProposeCandidateOnlyConsolidatesEligibleExactClaimEvidence(t *testing.T) {
	store := newFormationTestStore(t)
	seedAccountUsers(t, store, "other")
	base := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.2, "preference.drink", "tea")
	type staged struct {
		userID string
		key    string
		output memoryformation.CandidateOutput
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
	differentScope.Scope = memoryformation.ScopeShortTerm
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
	high := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.8, "preference.drink", "tea")
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
		loaded, err := store.LoadCandidate(context.Background(), cases[index].userID, candidate.ID)
		if err != nil || loaded.PublishedMemoryID != 0 || loaded.State != "proposed" {
			t.Fatalf("case=%s candidate=%+v err=%v", cases[index].key, loaded, err)
		}
	}
}

func TestProposeCandidateConsolidationReplayKeepsEvidenceCountStable(t *testing.T) {
	store := newFormationTestStore(t)
	low := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.2, "preference.drink", "tea")
	lowTurn := seedFormationTurn(t, store, "user", "replay", low.Evidence)
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: low, IdempotencyKey: "replay-low", Source: FormationSource{TurnID: lowTurn}}); err != nil {
		t.Fatal(err)
	}
	high := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.8, "preference.drink", "tea")
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
	base := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.6, "preference.drink", "tea")
	baseCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: base, IdempotencyKey: "target-base", Source: FormationSource{TurnID: seedFormationTurn(t, store, "user", "target", "I prefer tea")}})
	if err != nil {
		t.Fatal(err)
	}
	wrong := evaluatedClaimCandidate(t, "I use Go", "The user uses Go.", memoryformation.CategoryProjects, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.9, "project.language", "go")
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
	tea := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.8, "preference.drink", "tea")
	active, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: tea, IdempotencyKey: "tea"})
	if err != nil {
		t.Fatal(err)
	}
	coffee := evaluatedClaimCandidate(t, "I prefer coffee", "The user prefers coffee.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.7, "preference.drink", "coffee")
	blocked, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: coffee, IdempotencyKey: "weak-coffee"})
	if err != nil || blocked.State != "approved" || blocked.PublishedMemoryID != 0 {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
	replayed, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: coffee, IdempotencyKey: "weak-coffee"})
	if err != nil || created || replayed.PublishedMemoryID != 0 {
		t.Fatalf("blocked replay=%+v created=%v err=%v", replayed, created, err)
	}
	coffee = evaluatedClaimCandidate(t, "I prefer coffee", "The user prefers coffee.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.95, "preference.drink", "coffee")
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
	first := evaluatedClaimCandidate(t, "I enjoy tea", "The user enjoys tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.8, "durable_preferences.fact", "enjoys_tea")
	second := evaluatedClaimCandidate(t, "I enjoy hiking", "The user enjoys hiking.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.8, "durable_preferences.fact", "enjoys_hiking")
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
	tea := evaluatedFormationCandidate(t, "I prefer tea.", "I prefer tea.", "The user prefers tea.", memoryformation.CategoryDurablePreferences)
	active, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: tea, IdempotencyKey: "fallback-tea"})
	if err != nil {
		t.Fatal(err)
	}
	coffee := evaluatedFormationCandidate(t, "I prefer coffee.", "I prefer coffee.", "The user prefers coffee.", memoryformation.CategoryDurablePreferences)
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
	inferred := evaluatedClaimCandidate(t, "Considering pacman packages for file management.", "The user may use or be evaluating a pacman-based Arch-family Linux environment.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceModelInference, memoryformation.SensitivityLow, 0.9, "environment.linux_distribution", "arch_family")
	inferredMemory, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: inferred, IdempotencyKey: "inferred-arch"})
	if err != nil || inferredMemory.PublishedMemoryID == 0 {
		t.Fatalf("inferred=%+v err=%v", inferredMemory, err)
	}
	direct := inferred
	direct.Statement = "The user uses Arch Linux."
	direct.Evidence = "I use Arch Linux."
	direct.Provenance = memoryformation.ProvenanceUserStatement
	direct.SourceAuthority = memoryformation.AuthorityUserDirect
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
	low := evaluatedClaimCandidate(t, "I use Go", "The user uses Go.", memoryformation.CategoryProjects, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.2, "project.language", "go")
	first, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: low, IdempotencyKey: "low", Source: FormationSource{TurnID: turnID}})
	if err != nil || first.State != "proposed" || first.PublishedMemoryID != 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	high := low
	high.Approval = memoryformation.ApprovalApproved
	high.Confidence = 0.9
	merged, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: high, IdempotencyKey: "high", Source: FormationSource{TurnID: turnID}})
	if err != nil || created || merged.ID != first.ID || merged.State != "approved" || merged.PublishedMemoryID == 0 {
		t.Fatalf("merged=%+v created=%v err=%v", merged, created, err)
	}
}

func TestFormationLeaseFenceRollsBackCandidateAndCanonicalWrites(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	jobID, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.ID != jobID {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	job.LeaseUntil = job.LeaseUntil.Add(time.Second)
	output := evaluatedFormationCandidate(t, "I use Go", "I use Go", "The user uses Go.", memoryformation.CategoryProjects)
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, IdempotencyKey: "stale", Source: FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, FormationJob: &job}); err == nil {
		t.Fatal("expected stale lease")
	}
	var candidates, memories int
	_ = store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates`).Scan(&candidates)
	_ = store.sql.QueryRow(`SELECT COUNT(*) FROM memory_entries`).Scan(&memories)
	if candidates != 0 || memories != 0 {
		t.Fatalf("candidate=%d memory=%d", candidates, memories)
	}
}

func TestFormationLeaseRenewalAdvancesExactFence(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old := job
	leaseUntil, err := store.RenewFormationJobLease(context.Background(), job, 2*time.Minute)
	if err != nil || !leaseUntil.After(job.LeaseUntil) {
		t.Fatalf("renewed until=%s err=%v", leaseUntil, err)
	}
	if err := store.ValidateFormationJobLease(context.Background(), old); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("old lease validation=%v", err)
	}
	job.LeaseUntil = leaseUntil
	if err := store.ValidateFormationJobLease(context.Background(), job); err != nil {
		t.Fatalf("renewed lease validation=%v", err)
	}
}

func TestFormationJobsRequireExactSuccessfullyDeliveredSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Store, int64)
	}{
		{name: "undelivered", mutate: func(store *Store, turnID int64) {
			if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?`, turnID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "failed", mutate: func(store *Store, turnID int64) {
			if _, err := store.sql.Exec(`UPDATE session_turns SET delivery_failed_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), turnID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "deleted", mutate: func(store *Store, turnID int64) {
			if _, err := store.sql.Exec(`DELETE FROM session_turns WHERE id = ?`, turnID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newFormationTestStore(t)
			turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
			jobID, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user")
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(store, turnID)
			if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID, ExtractorVersion: "other"}, "user"); err == nil {
				t.Fatal("enqueued job from invalid source")
			}
			if _, err := store.ClaimFormationJob(context.Background(), time.Minute); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("claim invalid source job %d: %v", jobID, err)
			}
		})
	}

	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "wrong", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err == nil {
		t.Fatal("enqueued source with mismatched request identity")
	}
}

func TestReconcileFormationJobsOnlyUsesSuccessfulExactSources(t *testing.T) {
	store := newFormationTestStore(t)
	successful := seedFormationTurn(t, store, "user", "successful", "I use Go", "successful-request")
	undelivered := seedFormationTurn(t, store, "user", "undelivered", "I use Rust", "undelivered-request")
	failed := seedFormationTurn(t, store, "user", "failed", "I use Java", "failed-request")
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?; UPDATE session_turns SET delivery_failed_at = ? WHERE id = ?`, undelivered, formatTime(time.Now().UTC()), failed); err != nil {
		t.Fatal(err)
	}
	count, err := store.ReconcileFormationJobs(context.Background(), "model", FormationExtractorVersion)
	if err != nil || count != 1 {
		t.Fatalf("reconciled=%d err=%v", count, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.TurnID != successful || job.RequestID != "successful-request" || job.SessionID != "successful" || job.SessionGeneration != 1 {
		t.Fatalf("reconciled job=%+v", job)
	}
	if _, err := store.ClaimFormationJob(context.Background(), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claimed ineligible reconciled source: %v", err)
	}
}

func TestFormationLeaseMutationsFailAfterSourceBecomesUnsuccessful(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.LeaseOwner == "" {
		t.Fatal("claim returned empty lease owner")
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivery_failed_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), turnID); err != nil {
		t.Fatal(err)
	}
	assertStale := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrStaleFormationJobLease) {
			t.Fatalf("%s error=%v", name, err)
		}
	}
	_, err = store.FormationJobArtifact(context.Background(), job)
	assertStale("artifact", err)
	assertStale("validate", store.ValidateFormationJobLease(context.Background(), job))
	_, err = store.ReserveFormationModelSubmission(context.Background(), job)
	assertStale("reserve model submission", err)
	assertStale("save", store.SaveFormationJobArtifact(context.Background(), job, `{"memories":[]}`))
	output := evaluatedFormationCandidate(t, "I use Go", "I use Go", "The user uses Go.", memoryformation.CategoryProjects)
	_, _, err = store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, Source: FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, FormationJob: &job})
	assertStale("propose", err)
	assertStale("complete", store.CompleteFormationJob(context.Background(), job, false))
	assertStale("retry", store.RetryFormationJob(context.Background(), job, "transient", 5))
	assertStale("invalid retry", store.RetryInvalidFormationJob(context.Background(), job, "invalid_batch_shape"))
	assertStale("defer", store.DeferFormationJob(context.Background(), job, time.Second))
	assertStale("skip", store.SkipFormationJob(context.Background(), job, "invalid"))
}

func TestRetryInvalidFormationJobUsesDedicatedBudget(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.AttemptCount != 1 || job.InvalidOutputRetryCount != 0 {
		t.Fatalf("initial job=%+v", job)
	}
	if count, err := store.ReserveFormationModelSubmission(context.Background(), job); err != nil || count != 1 {
		t.Fatalf("reserve count=%d err=%v", count, err)
	} else {
		job.ModelSubmissionCount = count
	}
	if err := store.RetryInvalidFormationJob(context.Background(), job, "invalid_batch_shape"); err != nil {
		t.Fatal(err)
	}
	var state, leaseOwner, code string
	var attemptCount, invalidRetryCount, submissionCount int
	var leaseUntil sql.NullString
	if err := store.sql.QueryRow(`SELECT state, attempt_count, invalid_output_retry_count, model_submission_count, lease_owner, lease_until, corrective_error_code FROM durable_jobs WHERE id = ?`, job.ID).Scan(&state, &attemptCount, &invalidRetryCount, &submissionCount, &leaseOwner, &leaseUntil, &code); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || attemptCount != 1 || invalidRetryCount != 1 || submissionCount != 1 || leaseOwner != "" || leaseUntil.Valid || code != "invalid_batch_shape" {
		t.Fatalf("retried state=%q attempts=%d invalid_retries=%d submissions=%d lease_owner=%q lease_until=%v code=%q", state, attemptCount, invalidRetryCount, submissionCount, leaseOwner, leaseUntil, code)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.AttemptCount != 2 || reclaimed.InvalidOutputRetryCount != 1 || reclaimed.ModelSubmissionCount != 1 || reclaimed.CorrectiveErrorCode != "invalid_batch_shape" {
		t.Fatalf("reclaimed job=%+v", reclaimed)
	}
	if err := store.RetryInvalidFormationJob(context.Background(), reclaimed, "all_items_malformed"); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("second invalid retry error=%v", err)
	}
	if err := store.DeferFormationJob(context.Background(), reclaimed, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT attempt_count, invalid_output_retry_count, model_submission_count, corrective_error_code FROM durable_jobs WHERE id = ?`, job.ID).Scan(&attemptCount, &invalidRetryCount, &submissionCount, &code); err != nil {
		t.Fatal(err)
	}
	if code != "invalid_batch_shape" {
		t.Fatalf("deferred structured retry lost corrective reason: %q", code)
	}
	if attemptCount != 1 || invalidRetryCount != 1 || submissionCount != 1 {
		t.Fatalf("preempted attempts=%d invalid_retries=%d submissions=%d", attemptCount, invalidRetryCount, submissionCount)
	}
}

func TestFormationModelSubmissionBudgetCannotBeResetByClaims(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for submission := 1; submission <= DurableModelSubmissionLimit; submission++ {
		count, reserveErr := store.ReserveFormationModelSubmission(context.Background(), job)
		if reserveErr != nil || count != submission {
			t.Fatalf("submission=%d count=%d err=%v", submission, count, reserveErr)
		}
		job.ModelSubmissionCount = count
		if submission == DurableModelSubmissionLimit {
			break
		}
		if err := store.RetryFormationJob(context.Background(), job, "transient_provider", DurableModelSubmissionLimit); err != nil {
			t.Fatal(err)
		}
		if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
			t.Fatal(err)
		}
		job, err = store.ClaimFormationJob(context.Background(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ReserveFormationModelSubmission(context.Background(), job); !errors.Is(err, ErrModelSubmissionBudgetExhausted) {
		t.Fatalf("fourth reservation error=%v", err)
	}
}

func TestInvalidOutputRetrySurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := NewStore(path, config.NewLogger(config.LevelError))
	seedAccountUsers(t, store, "user")
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	count, err := store.ReserveFormationModelSubmission(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	job.ModelSubmissionCount = count
	if err := store.RetryInvalidFormationJob(context.Background(), job, "missing_tool_call"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := NewStore(path, config.NewLogger(config.LevelError))
	t.Cleanup(func() { reopened.Close() })
	if _, err := reopened.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := reopened.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.InvalidOutputRetryCount != 1 || reclaimed.AttemptCount != 2 || reclaimed.ModelSubmissionCount != 1 || reclaimed.CorrectiveErrorCode != "missing_tool_call" {
		t.Fatalf("reopened job=%+v", reclaimed)
	}
}

func TestReclaimedFormationLeaseRejectsStaleOwnerAtExactLeaseUntil(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	stale, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), stale.ID); err != nil {
		t.Fatal(err)
	}
	current, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if current.LeaseOwner == stale.LeaseOwner || current.LeaseOwner == "" {
		t.Fatalf("stale owner=%q current owner=%q", stale.LeaseOwner, current.LeaseOwner)
	}
	stale.LeaseUntil = current.LeaseUntil
	if err := store.SaveFormationJobArtifact(context.Background(), stale, `{"memories":[]}`); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("stale reclaimed lease saved artifact: %v", err)
	}
	if err := store.SaveFormationJobArtifact(context.Background(), current, `{"memories":[]}`); err != nil {
		t.Fatalf("current lease failed: %v", err)
	}
}

func newFormationTestStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	seedAccountUsers(t, store, "user")
	return store
}

func evaluatedFormationCandidate(t *testing.T, source, evidence, statement string, category memoryformation.Category) memoryformation.CandidateOutput {
	t.Helper()
	output, err := memoryformation.Evaluate(memoryformation.CandidateInput{SourceUserText: source, Statement: statement, Evidence: evidence, Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect, Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModeAutomaticExtraction, Scope: memoryformation.ScopeLongTerm, Category: category, Context: memoryformation.ContextDirectAssertion, Confidence: 0.9, Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func evaluatedClaimCandidate(t *testing.T, source, statement string, category memoryformation.Category, provenance memoryformation.Provenance, sensitivity memoryformation.Sensitivity, confidence float64, claimSlot, claimValue string) memoryformation.CandidateOutput {
	t.Helper()
	claimedAuthority := memoryformation.AuthorityModel
	if provenance == memoryformation.ProvenanceUserStatement {
		claimedAuthority = memoryformation.AuthorityUserDirect
	}
	output, err := memoryformation.Evaluate(memoryformation.CandidateInput{SourceUserText: source, Statement: statement, Evidence: source, Provenance: provenance, ClaimedAuthority: claimedAuthority, Sensitivity: sensitivity, Mode: memoryformation.ModeAutomaticExtraction, Scope: memoryformation.ScopeLongTerm, Category: category, Context: memoryformation.ContextDirectAssertion, Confidence: confidence, Importance: 4, ClaimSlot: claimSlot, ClaimValue: claimValue})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func seedFormationTurn(t *testing.T, store *Store, userID, sessionID, userText string, requestID ...string) int64 {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), userID, sessionID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if len(requestID) > 0 {
		ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: requestID[0]})
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, sessionID, userID, profile.Generation, userText, "answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(context.Background(), userID, turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}
