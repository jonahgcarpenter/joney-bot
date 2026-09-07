package usermemory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestCorrectionLookupExposesCanonicalRevisionAndStagesValidSave(t *testing.T) {
	store, db := newHandlerTestStore(t, fixedRecallEmbedder{})
	seedHandlerUser(t, db, "user")
	entry, err := memorytest.PublishMemory(context.Background(), store, "user", memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Category: "identity", Statement: "The user lives in Porto.", Evidence: "I live in Porto.", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	rebuildHandlerIndexes(t, store, true)
	// Metadata changes after indexing must hydrate from canonical state.
	const assessmentContext = "Historical residence, before 2020; not current."
	wantRevision := entry.Revision + 1
	if _, err := db.Exec(`UPDATE memory_entries SET assessment_context = ? WHERE id = ?`, assessmentContext, entry.ID); err != nil {
		t.Fatal(err)
	}
	entry, err = store.EntryByID(entry.ID)
	if err != nil || entry.Revision != wantRevision || entry.Context != assessmentContext {
		t.Fatalf("canonical read: %+v %v", entry, err)
	}
	ctx := principalContext("user", "external")
	log := config.NewLogger(config.LevelError)
	list, err := NewListHandler(store, log)(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{fmt.Sprintf("Memory ID: %d", entry.ID), fmt.Sprintf("Revision: %d", wantRevision), "Claim slot: " + fmt.Sprintf("%q", entry.ClaimSlot), "Claim value: " + fmt.Sprintf("%q", entry.ClaimValue), "Context: " + fmt.Sprintf("%q", assessmentContext)} {
		if !strings.Contains(list.Content, want) {
			t.Fatalf("list missing %q: %s", want, list.Content)
		}
	}
	for _, channel := range []string{"hybrid", "semantic"} {
		if channel == "semantic" {
			live, err := store.LiveIndexRevision(ctx, memory.IndexKindMemoryFTS)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DROP TABLE ` + live.TableName); err != nil {
				t.Fatal(err)
			}
		}
		search, err := NewSearchHandler(store, log)(ctx, map[string]interface{}{"query": "Porto"})
		if err != nil {
			t.Fatal(err)
		}
		var target struct {
			ID         int64  `json:"id"`
			Revision   int64  `json:"revision"`
			ClaimSlot  string `json:"claim_slot"`
			ClaimValue string `json:"claim_value"`
			Context    string `json:"assessment_context"`
		}
		for _, line := range strings.Split(search.Content, "\n") {
			if strings.HasPrefix(line, "{") {
				if err := json.Unmarshal([]byte(line), &target); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		if target.ID != entry.ID || target.Revision != wantRevision || target.ClaimSlot != entry.ClaimSlot || target.ClaimValue != entry.ClaimValue || target.Context != assessmentContext {
			t.Fatalf("%s lookup: %+v", channel, target)
		}
		collector := requestctx.NewMemoryStageCollector()
		saveCtx := requestctx.WithMemoryStageCollector(requestctx.WithMetadata(ctx, requestctx.Metadata{CurrentUserText: "I now live in Lisbon."}), collector)
		_, err = NewSaveHandler(log)(saveCtx, map[string]interface{}{"memories": []interface{}{map[string]interface{}{
			"statement": "The user now lives in Lisbon.", "evidence": "I now live in Lisbon.", "category": entry.Category,
			"claim_slot": target.ClaimSlot, "claim_value": "lisbon", "supersedes": entry.Statement, "evidence_type": "direct_statement", "confidence": .7,
			"intent": "correction", "retention": "durable", "target_memory_id": target.ID, "expected_revision": target.Revision, "cardinality": "single",
		}}})
		if err != nil || len(collector.Candidates()) != 1 {
			t.Fatalf("correction staging: %v", err)
		}
		encoded, err := memory.EncodeForegroundMemory("user", collector.Candidates())
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := memory.DecodeForegroundMemory(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := memory.ValidateAssessmentBatch(memory.AssessmentBatch{Version: 1, Items: artifact.Candidates}); err != nil {
			t.Fatalf("staged correction fails core contract: %v", err)
		}
	}
}

func TestSaveCorrectionRequiresLookupButReinforcementDoesNot(t *testing.T) {
	for _, intent := range []string{"correction", "retire", "automatic", "remember"} {
		t.Run(intent, func(t *testing.T) {
			collector := requestctx.NewMemoryStageCollector()
			ctx := requestctx.WithMemoryStageCollector(requestctx.WithMetadata(principalContext("user", "external"), requestctx.Metadata{CurrentUserText: "I prefer tea."}), collector)
			result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{"memories": []interface{}{map[string]interface{}{
				"statement": "The user prefers tea.", "evidence": "I prefer tea.", "category": "durable_preferences", "claim_slot": "preference.drink", "claim_value": "tea", "supersedes": "The user prefers coffee.", "evidence_type": "direct_statement", "confidence": .8, "intent": intent, "retention": "durable", "reinforces_memory_id": 17,
			}}})
			if err != nil {
				t.Fatal(err)
			}
			if intent == "correction" || intent == "retire" {
				if len(collector.Candidates()) != 0 || !strings.Contains(result.Content, `"reason_code":"missing_target_revision"`) || !strings.Contains(result.Content, `"retryable":true`) || !strings.Contains(result.Content, "user_memory_search") {
					t.Fatalf("missing lookup rejection: %s", result.Content)
				}
			} else {
				if len(collector.Candidates()) != 1 || collector.Candidates()[0].ExpectedRevision != 0 {
					t.Fatalf("reinforcement required revision: %s", result.Content)
				}
				encoded, err := memory.EncodeForegroundMemory("user", collector.Candidates())
				if err != nil {
					t.Fatal(err)
				}
				artifact, err := memory.DecodeForegroundMemory(encoded)
				if err != nil {
					t.Fatal(err)
				}
				if err := memory.ValidateAssessmentBatch(memory.AssessmentBatch{Version: 1, Items: artifact.Candidates}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
