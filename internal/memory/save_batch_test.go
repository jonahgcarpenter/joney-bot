package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

func TestDecodeMemorySaveBatchRequiresEveryField(t *testing.T) {
	for _, field := range MemorySaveRequiredFields() {
		for _, value := range []string{"missing", "null"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				item := validMemorySaveMap()
				if value == "missing" {
					delete(item, field)
				} else {
					item[field] = nil
				}
				batch, itemErrors, err := DecodeMemorySaveBatch(map[string]interface{}{"memories": []interface{}{item}})
				if err != nil || len(batch.Memories) != 0 || len(itemErrors) != 1 || itemErrors[0].InputIndex != 0 {
					t.Fatalf("batch=%+v item_errors=%+v err=%v", batch, itemErrors, err)
				}
			})
		}
	}
}

func TestDecodeMemorySaveBatchPreservesValidZeroValuesAndSiblings(t *testing.T) {
	valid := validMemorySaveMap()
	valid["confidence"] = 0.0
	malformed := validMemorySaveMap()
	delete(malformed, "category")
	batch, itemErrors, err := DecodeMemorySaveBatch(map[string]interface{}{"memories": []interface{}{malformed, valid}})
	if err != nil || len(itemErrors) != 1 || len(batch.Memories) != 1 || batch.Memories[0].InputIndex != 1 || batch.Memories[0].Confidence != 0 || batch.Memories[0].TTLDays != 0 || batch.Memories[0].Supersedes != "" {
		t.Fatalf("batch=%+v item_errors=%+v err=%v", batch, itemErrors, err)
	}
}

func TestDecodeMemorySaveBatchRejectsSchemaRangeViolations(t *testing.T) {
	for field, value := range map[string]interface{}{"confidence": 1.1, "importance": 6, "ttl_days": 31} {
		t.Run(field, func(t *testing.T) {
			item := validMemorySaveMap()
			item[field] = value
			batch, itemErrors, err := DecodeMemorySaveBatch(map[string]interface{}{"memories": []interface{}{item}})
			if err != nil || len(batch.Memories) != 0 || len(itemErrors) != 1 {
				t.Fatalf("batch=%+v item_errors=%+v err=%v", batch, itemErrors, err)
			}
		})
	}
}

func TestDecodeMemorySaveBatchRejectsSchemaEnumViolations(t *testing.T) {
	for _, field := range []string{"scope", "category", "context", "provenance", "sensitivity"} {
		t.Run(field, func(t *testing.T) {
			item := validMemorySaveMap()
			item[field] = "not_in_schema"
			batch, itemErrors, err := DecodeMemorySaveBatch(map[string]interface{}{"memories": []interface{}{item}})
			if err != nil || len(batch.Memories) != 0 || batch.SubmittedCount != 1 || batch.MalformedCount != 1 || len(itemErrors) != 1 {
				t.Fatalf("batch=%+v item_errors=%+v err=%v", batch, itemErrors, err)
			}
		})
	}
}

func TestDecodeMemorySaveBatchJSONUsesSameContract(t *testing.T) {
	data := []byte(fmt.Sprintf(`{"memories":[%s]}`, mustMemorySaveJSON(t, validMemorySaveMap())))
	batch, itemErrors, err := DecodeMemorySaveBatchJSON(data)
	if err != nil || len(itemErrors) != 0 || len(batch.Memories) != 1 {
		t.Fatalf("batch=%+v item_errors=%+v err=%v", batch, itemErrors, err)
	}
	if _, _, err := DecodeMemorySaveBatchJSON([]byte(`{"memories":[],"extra":true}`)); err == nil {
		t.Fatal("unknown outer field was accepted")
	}
}

func TestMemorySaveBatchArtifactPreservesMixedBatchCounts(t *testing.T) {
	malformed := validMemorySaveMap()
	delete(malformed, "category")
	batch, itemErrors, err := DecodeMemorySaveBatch(map[string]interface{}{"memories": []interface{}{malformed, validMemorySaveMap()}})
	if err != nil || len(itemErrors) != 1 || batch.SubmittedCount != 2 || batch.MalformedCount != 1 || len(batch.Memories) != 1 {
		t.Fatalf("batch=%+v item_errors=%+v err=%v", batch, itemErrors, err)
	}
	artifact, err := MarshalMemorySaveBatchArtifact(batch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(artifact, []byte(`"submitted_count":2`)) || !bytes.Contains(artifact, []byte(`"malformed_count":1`)) {
		t.Fatalf("artifact omitted counts: %s", artifact)
	}
	replayed, replayErrors, err := DecodeMemorySaveBatchJSON(artifact)
	if err != nil || len(replayErrors) != 0 || replayed.SubmittedCount != 2 || replayed.MalformedCount != 1 || len(replayed.Memories) != 1 {
		t.Fatalf("replayed=%+v item_errors=%+v err=%v", replayed, replayErrors, err)
	}
}

func TestMemorySaveBatchArtifactAcceptsLegacyShape(t *testing.T) {
	malformed := validMemorySaveMap()
	delete(malformed, "category")
	artifact := []byte(fmt.Sprintf(`{"memories":[%s,%s]}`, mustMemorySaveJSON(t, malformed), mustMemorySaveJSON(t, validMemorySaveMap())))
	batch, itemErrors, err := DecodeMemorySaveBatchJSON(artifact)
	if err != nil || len(itemErrors) != 1 || batch.SubmittedCount != 2 || batch.MalformedCount != 1 || len(batch.Memories) != 1 {
		t.Fatalf("batch=%+v item_errors=%+v err=%v", batch, itemErrors, err)
	}
}

func TestMemorySaveBatchArtifactRoundTripsEmptyAndLegacyNull(t *testing.T) {
	artifact, err := MarshalMemorySaveBatchArtifact(MemorySaveBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(artifact, []byte(`"memories":[]`)) {
		t.Fatalf("empty artifact did not normalize memories: %s", artifact)
	}
	for name, data := range map[string][]byte{"current": artifact, "legacy null": []byte(`{"memories":null}`)} {
		t.Run(name, func(t *testing.T) {
			batch, itemErrors, err := DecodeMemorySaveBatchJSON(data)
			if err != nil || len(itemErrors) != 0 || batch.SubmittedCount != 0 || batch.MalformedCount != 0 || len(batch.Memories) != 0 {
				t.Fatalf("batch=%+v item_errors=%+v err=%v", batch, itemErrors, err)
			}
		})
	}
	if _, _, err := DecodeMemorySaveBatch(map[string]interface{}{"memories": nil}); err == nil {
		t.Fatal("model-facing null memories was accepted")
	}
}

func TestMemorySaveBatchArtifactRejectsInvalidMetadata(t *testing.T) {
	valid := mustMemorySaveJSON(t, validMemorySaveMap())
	for name, artifact := range map[string]string{
		"missing malformed":   `{"memories":[],"submitted_count":0}`,
		"missing submitted":   `{"memories":[],"malformed_count":0}`,
		"unknown field":       `{"memories":[],"submitted_count":0,"malformed_count":0,"extra":true}`,
		"string count":        `{"memories":[],"submitted_count":"0","malformed_count":0}`,
		"fractional count":    `{"memories":[],"submitted_count":0.5,"malformed_count":0}`,
		"negative count":      `{"memories":[],"submitted_count":-1,"malformed_count":0}`,
		"above maximum":       `{"memories":[],"submitted_count":6,"malformed_count":6}`,
		"malformed above all": `{"memories":[],"submitted_count":0,"malformed_count":1}`,
		"count mismatch":      `{"memories":[` + valid + `],"submitted_count":2,"malformed_count":0}`,
		"trailing JSON":       `{"memories":[],"submitted_count":0,"malformed_count":0}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeMemorySaveBatchJSON([]byte(artifact)); err == nil {
				t.Fatal("invalid artifact was accepted")
			}
		})
	}
	for name, batch := range map[string]MemorySaveBatch{
		"missing submitted":  {Memories: []MemorySaveItem{{}}},
		"negative malformed": {SubmittedCount: 0, MalformedCount: -1},
		"above maximum":      {SubmittedCount: 6, MalformedCount: 6},
		"count mismatch":     {Memories: []MemorySaveItem{{}}, SubmittedCount: 2},
	} {
		t.Run("marshal "+name, func(t *testing.T) {
			if _, err := MarshalMemorySaveBatchArtifact(batch); err == nil {
				t.Fatal("invalid in-memory batch was accepted")
			}
		})
	}
}

func TestSubmitMemorySaveBatchPreservesExplicitRememberModeFromUserWording(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { store.Close() })
	seedAccountUsers(t, store, "user-1")

	outcomes := store.SubmitMemorySaveBatch(context.Background(), "user-1", "Remember that I prefer concise replies.", FormationSource{RequestID: "explicit"}, MemorySaveBatch{Memories: []MemorySaveItem{{
		Statement: "The user prefers concise replies.", Evidence: "I prefer concise replies.",
		Scope: "long_term", Category: "communication_preferences", Context: "direct_assertion",
		Provenance: "user_statement", Sensitivity: "low", Confidence: 0.95, Importance: 4,
		ClaimSlot: "communication.reply_style", ClaimValue: "concise",
	}}}, nil)
	if len(outcomes) != 1 || outcomes[0].Err != nil {
		t.Fatalf("outcomes=%+v", outcomes)
	}
	candidate, err := store.loadFixtureCandidate(context.Background(), "user-1", outcomes[0].CandidateID)
	if err != nil || candidate.FormationMode != string(policy.ModeExplicitRemember) {
		t.Fatalf("candidate=%+v err=%v", candidate, err)
	}
}

func validMemorySaveMap() map[string]interface{} {
	return map[string]interface{}{
		"statement": "The user uses Go.", "evidence": "I use Go.", "scope": "long_term", "category": "projects",
		"context": "direct_assertion", "provenance": "user_statement", "sensitivity": "low", "confidence": 0.9,
		"importance": 4, "ttl_days": 0, "supersedes": "", "claim_slot": "project.language", "claim_value": "go",
	}
}

func mustMemorySaveJSON(t *testing.T, item map[string]interface{}) string {
	t.Helper()
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
