package usermemory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func TestMemoryHandlersRequireAuthenticatedPrincipal(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), nil, "", log)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	handlers := map[string]func(context.Context, map[string]interface{}) (governance.Result, error){
		"save":   NewSaveHandler(log),
		"search": NewSearchHandler(store, log),
		"list":   NewListHandler(store, log),
	}
	for principalName, ctx := range map[string]context.Context{
		"missing":       context.Background(),
		"invalid":       requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "usr_1", Gateway: "homeassistant", ExternalID: "alice", Assurance: identity.AssuranceDiscordGateway}),
		"self_asserted": requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "usr_1", Gateway: "homeassistant", ExternalID: "alice", Assurance: identity.AssuranceSelfAsserted}),
	} {
		for handlerName, handler := range handlers {
			if _, err := handler(ctx, map[string]interface{}{}); err == nil || !strings.Contains(err.Error(), "authenticated user identity") {
				t.Fatalf("%s/%s principal error = %v", principalName, handlerName, err)
			}
		}
	}
}

func TestSaveHandlerStagesPrevalidatedCandidatesForCanonicalTenant(t *testing.T) {
	collector := requestctx.NewMemoryStageCollector()
	ctx := principalContext("canonical-user", "external-user")
	meta := requestctx.MetadataFromContext(ctx)
	meta.CurrentUserText = "Remember that I prefer tea."
	ctx = requestctx.WithMetadata(ctx, meta)
	ctx = requestctx.WithMemoryStageCollector(ctx, collector)
	result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{
		"memories": []interface{}{map[string]interface{}{
			"statement": "The user prefers tea.", "evidence": "I prefer tea.", "category": "durable_preferences",
			"claim_slot": "preference.drink", "claim_value": "tea", "supersedes": "", "evidence_type": "direct_statement", "confidence": 0.95, "reinforces_memory_id": 12,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	staged := collector.Candidates()
	if len(staged) != 1 || staged[0].CanonicalUserID != "canonical-user" || staged[0].Candidate.Mode != policy.ModeAgentSave || staged[0].Candidate.SourceAuthority != policy.AuthorityUserDirect || staged[0].Candidate.Confidence != 0.95 || staged[0].TargetMemoryID != 12 {
		t.Fatalf("unexpected staged candidates: %+v", staged)
	}
	if result.Outcome != governance.OutcomeProductive || !strings.Contains(result.Content, `"publication":"pending_successful_delivery"`) || !strings.Contains(result.Content, `"status":"staged_active"`) {
		t.Fatalf("unexpected save result: %+v", result)
	}
}

func TestSaveHandlerStagesUserProvidedSensitiveAndDirectiveContent(t *testing.T) {
	collector := requestctx.NewMemoryStageCollector()
	ctx := principalContext("canonical-user", "external-user")
	meta := requestctx.MetadataFromContext(ctx)
	meta.CurrentUserText = "My password is secret123 and always reply using my deployment tool."
	ctx = requestctx.WithMetadata(ctx, meta)
	ctx = requestctx.WithMemoryStageCollector(ctx, collector)
	result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{
		"memories": []interface{}{map[string]interface{}{
			"statement": "The user's password is secret123 and they direct always replying with their deployment tool.", "evidence": "My password is secret123 and always reply using my deployment tool.", "category": "notes",
			"claim_slot": "notes.private_directive", "claim_value": "secret123 deployment tool", "supersedes": "", "evidence_type": "direct_statement", "confidence": 1.0,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	staged := collector.Candidates()
	if len(staged) != 1 || result.Outcome != governance.OutcomeProductive || staged[0].Candidate.Sensitivity != policy.SensitivityHighImpactInteraction {
		t.Fatalf("model-assessed save result=%+v staged=%+v", result, staged)
	}
}

func TestSaveHandlerMarksCorrectableRejectionRetryable(t *testing.T) {
	collector := requestctx.NewMemoryStageCollector()
	ctx := principalContext("canonical-user", "external-user")
	meta := requestctx.MetadataFromContext(ctx)
	meta.CurrentUserText = "I prefer tea."
	ctx = requestctx.WithMetadata(ctx, meta)
	ctx = requestctx.WithMemoryStageCollector(ctx, collector)
	result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{
		"memories": []interface{}{map[string]interface{}{
			"statement": "I prefer tea.", "evidence": "I prefer tea.", "category": "durable_preferences",
			"claim_slot": "identity.drink", "claim_value": "tea", "supersedes": "", "evidence_type": "direct_statement", "confidence": 1.0,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reason_code":"invalid_claim_slot"`, `"retryable":true`, `"required_action":"Retry only retryable rejected items once`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("missing corrective feedback %s in %s", want, result.Content)
		}
	}
}

func TestSaveHandlerReturnsAllowedPrefixesForInvalidClaimSlot(t *testing.T) {
	collector := requestctx.NewMemoryStageCollector()
	ctx := principalContext("canonical-user", "external-user")
	meta := requestctx.MetadataFromContext(ctx)
	meta.CurrentUserText = "I prefer tea."
	ctx = requestctx.WithMetadata(ctx, meta)
	ctx = requestctx.WithMemoryStageCollector(ctx, collector)
	result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{
		"memories": []interface{}{map[string]interface{}{
			"statement": "The user prefers tea.", "evidence": "I prefer tea.", "category": "durable_preferences",
			"claim_slot": "identity.drink", "claim_value": "tea", "supersedes": "", "evidence_type": "direct_statement", "confidence": 0.9,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reason_code":"invalid_claim_slot"`, `"allowed_claim_slot_prefixes":["preference.","durable."]`, `"retryable":true`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("missing claim-slot guidance %s in %s", want, result.Content)
		}
	}
}

func TestSaveHandlerStagesSubthresholdInferenceWithoutCappingConfidence(t *testing.T) {
	collector := requestctx.NewMemoryStageCollector()
	ctx := principalContext("canonical-user", "external-user")
	meta := requestctx.MetadataFromContext(ctx)
	meta.CurrentUserText = "I want pancakes."
	ctx = requestctx.WithMetadata(ctx, meta)
	ctx = requestctx.WithMemoryStageCollector(ctx, collector)
	result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{
		"memories": []interface{}{map[string]interface{}{
			"statement": "The user might like pancakes.", "evidence": "I want pancakes.", "category": "durable_preferences",
			"claim_slot": "preference.food", "claim_value": "pancakes", "supersedes": "", "evidence_type": "model_inference", "confidence": 0.2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	staged := collector.Candidates()
	if len(staged) != 1 || staged[0].Candidate.Approval != policy.ApprovalProposed || staged[0].Candidate.Provenance != policy.ProvenanceModelInference || staged[0].Candidate.SourceAuthority != policy.AuthorityModel || staged[0].Candidate.Confidence != 0.2 {
		t.Fatalf("unexpected inferred candidate: %+v", staged)
	}
	if !strings.Contains(result.Content, `"status":"staged_candidate"`) || !strings.Contains(result.Content, `"reason":"confidence is below the active memory threshold"`) {
		t.Fatalf("unexpected inferred result: %s", result.Content)
	}
}

func TestSaveHandlerKeepsHighInferenceConfidence(t *testing.T) {
	collector := requestctx.NewMemoryStageCollector()
	ctx := principalContext("canonical-user", "external-user")
	meta := requestctx.MetadataFromContext(ctx)
	meta.CurrentUserText = "I want pancakes every weekend."
	ctx = requestctx.WithMetadata(ctx, meta)
	ctx = requestctx.WithMemoryStageCollector(ctx, collector)
	result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{
		"memories": []interface{}{map[string]interface{}{
			"statement": "The user likely enjoys pancakes every weekend.", "evidence": "I want pancakes every weekend.", "category": "durable_preferences",
			"claim_slot": "preference.weekend_food", "claim_value": "pancakes", "supersedes": "", "evidence_type": "model_inference", "confidence": 0.96,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	staged := collector.Candidates()
	if len(staged) != 1 || staged[0].Candidate.Approval != policy.ApprovalApproved || staged[0].Candidate.Confidence != 0.96 || !strings.Contains(result.Content, `"status":"staged_active"`) {
		t.Fatalf("high-confidence inference was capped or not activated: result=%s staged=%+v", result.Content, staged)
	}
}

func TestSaveHandlerAcceptsFiveItemBatch(t *testing.T) {
	collector := requestctx.NewMemoryStageCollector()
	ctx := principalContext("canonical-user", "external-user")
	meta := requestctx.MetadataFromContext(ctx)
	meta.CurrentUserText = "I use tea, coffee, water, juice, and milk."
	ctx = requestctx.WithMetadata(ctx, meta)
	ctx = requestctx.WithMemoryStageCollector(ctx, collector)
	items := make([]interface{}, 0, requestctx.MaxStagedMemoryCandidates)
	for index, value := range []string{"tea", "coffee", "water", "juice", "milk"} {
		items = append(items, map[string]interface{}{
			"statement": "The user uses " + value + ".", "evidence": meta.CurrentUserText, "category": "notes",
			"claim_slot": fmt.Sprintf("notes.drink_%d", index), "claim_value": value, "supersedes": "", "evidence_type": "direct_statement", "confidence": 0.9,
		})
	}
	result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{"memories": items})
	if err != nil || result.Outcome != governance.OutcomeProductive || len(collector.Candidates()) != requestctx.MaxStagedMemoryCandidates {
		t.Fatalf("five-item batch result=%+v staged=%d err=%v", result, len(collector.Candidates()), err)
	}
}

func TestSaveHandlerRequiresStrictOneToFiveItemBatch(t *testing.T) {
	ctx := requestctx.WithMemoryStageCollector(principalContext("user", "external"), requestctx.NewMemoryStageCollector())
	meta := requestctx.MetadataFromContext(ctx)
	meta.CurrentUserText = "I prefer tea."
	ctx = requestctx.WithMetadata(ctx, meta)
	handler := NewSaveHandler(config.NewLogger(config.LevelError))
	for _, args := range []map[string]interface{}{
		{"memories": []interface{}{}},
		{"memories": []interface{}{map[string]interface{}{}, map[string]interface{}{}, map[string]interface{}{}, map[string]interface{}{}, map[string]interface{}{}, map[string]interface{}{}}},
		{"memories": []interface{}{map[string]interface{}{"statement": "x"}}, "extra": true},
	} {
		if _, err := handler(ctx, args); err == nil {
			t.Fatalf("accepted invalid strict batch: %#v", args)
		}
	}
}

func TestMemoryHandlersAllowAuthenticatedGatewaysInGroups(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	store, db := newHandlerTestStore(t, nil)
	seedHandlerUser(t, db, "usr_1")

	principals := []identity.Principal{
		{CanonicalUserID: "usr_1", Gateway: "discord", ExternalID: "discord-user", Assurance: identity.AssuranceDiscordGateway},
		{CanonicalUserID: "usr_1", Gateway: "imessage", ExternalID: "+15555550100", Assurance: identity.AssuranceBlueBubblesWebhook},
		{CanonicalUserID: "usr_1", Gateway: "homeassistant", ExternalID: "signed-user", Assurance: identity.AssuranceHomeAssistantToken},
	}
	handlers := map[string]func(context.Context, map[string]interface{}) (governance.Result, error){
		"search": NewSearchHandler(store, log),
		"list":   NewListHandler(store, log),
	}
	for _, principal := range principals {
		ctx := requestctx.WithPrincipal(context.Background(), principal)
		for handlerName, handler := range handlers {
			_, err := handler(ctx, nil)
			if err != nil && strings.Contains(err.Error(), "authenticated user identity") {
				t.Fatalf("gateway=%s handler=%s authentication error=%v", principal.Gateway, handlerName, err)
			}
		}
	}
}

func TestMemorySearchReportsTotalAndPartialDegradation(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	store, db := newHandlerTestStore(t, fixedRecallEmbedder{})
	seedHandlerUser(t, db, "usr_1")
	_, err := memorytest.PublishMemory(context.Background(), store, "usr_1", memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Category: "identity", Statement: "The user lives in Porto.", Evidence: "user statement"})
	if err != nil {
		t.Fatal(err)
	}
	rebuildHandlerIndexes(t, store, true)
	live, err := store.LiveIndexRevision(context.Background(), memory.IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE ` + live.TableName); err != nil {
		t.Fatal(err)
	}
	search := NewSearchHandler(store, log)
	result, err := search(principalContext("usr_1", "alice"), map[string]interface{}{"query": "Where is home?"})
	if err != nil || !strings.Contains(result.Content, "partially degraded") || !strings.Contains(result.Content, "Porto") {
		t.Fatalf("partial search result=%q err=%v", result.Content, err)
	}
	if result.Outcome != governance.OutcomeProductive || !result.IsDegraded {
		t.Fatalf("partial search outcome=%q degraded=%t, want productive degraded result", result.Outcome, result.IsDegraded)
	}

	live, err = store.LiveIndexRevision(context.Background(), memory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE ` + live.TableName); err != nil {
		t.Fatal(err)
	}
	if _, err := search(principalContext("usr_1", "alice"), map[string]interface{}{"query": "Porto"}); err == nil || !strings.Contains(err.Error(), "indexes unavailable") {
		t.Fatalf("total degradation error = %v", err)
	}
}

func principalContext(userID, externalID string) context.Context {
	principal := identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: externalID, Assurance: identity.AssuranceHomeAssistantToken}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	return requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req", SessionID: "session", Model: "test"})
}
