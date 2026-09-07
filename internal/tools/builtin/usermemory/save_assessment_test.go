package usermemory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestSaveHandlerAssessmentMetadataAndNoActiveObservationPromise(t *testing.T) {
	for _, intent := range []string{"automatic", "remember", "retire"} {
		t.Run(intent, func(t *testing.T) {
			collector := requestctx.NewMemoryStageCollector()
			ctx := requestctx.WithMetadata(principalContext("user", "external"), requestctx.Metadata{CurrentUserText: "For this task\n I use C++."})
			ctx = requestctx.WithMemoryStageCollector(ctx, collector)
			item := map[string]interface{}{"statement": "The user uses C++ for this task.", "evidence": "For this task\n I use C++.", "category": "environment", "claim_slot": "environment.language", "claim_value": "C++", "supersedes": "", "evidence_type": "direct_statement", "confidence": .4, "retention": "observation", "intent": intent, "context": "Current task only.", "ttl_days": 0, "cardinality": "multiple", "target_memory_id": 17, "expected_revision": 3}
			result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{"memories": []interface{}{item}})
			if err != nil {
				t.Fatal(err)
			}
			staged := collector.Candidates()
			if len(staged) != 1 || staged[0].Candidate.Approval != policy.ApprovalProposed || staged[0].Candidate.ClaimValue != "c++" || staged[0].Candidate.Evidence != item["evidence"] || staged[0].TTLDays != 7 || staged[0].Context != "Current task only." || staged[0].Intent != intent || staged[0].ExpectedRevision != 3 {
				t.Fatalf("staged metadata: %+v", staged)
			}
			if strings.Contains(result.Content, "staged_active") || !strings.Contains(result.Content, "not a promise of active memory") {
				t.Fatalf("active promise: %s", result.Content)
			}
			encoded, err := memory.EncodeForegroundMemory("user", staged)
			if err != nil {
				t.Fatal(err)
			}
			artifact, err := memory.DecodeForegroundMemory(encoded)
			if err != nil || artifact.Candidates[0].ExpectedRevision != 3 || artifact.Candidates[0].Intent != intent || artifact.Candidates[0].Evidence != item["evidence"] {
				t.Fatalf("lost durable fields: %+v %v", artifact, err)
			}
		})
	}
}

func TestSaveHandlerRejectsUntrustedReferencesAndNonCurrentEvidence(t *testing.T) {
	for _, kind := range []string{"observation refs", "conflicting IDs", "nonexact", "retire inference", "retire no target", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			collector := requestctx.NewMemoryStageCollector()
			ctx := requestctx.WithMemoryStageCollector(requestctx.WithMetadata(principalContext("user", "external"), requestctx.Metadata{CurrentUserText: "I no longer like tea."}), collector)
			item := map[string]interface{}{"statement": "The user no longer likes tea.", "evidence": "I no longer like tea.", "category": "durable_preferences", "claim_slot": "preference.drink", "claim_value": "tea", "supersedes": "", "evidence_type": "direct_statement", "confidence": .9, "retention": "durable", "intent": "retire", "target_memory_id": 17}
			switch kind {
			case "observation refs":
				item["source_observation_ids"] = []int64{1}
			case "conflicting IDs":
				item["reinforces_memory_id"] = 18
			case "nonexact":
				item["evidence"] = "I like tea."
			case "retire inference":
				item["evidence_type"] = "model_inference"
			case "retire no target":
				delete(item, "target_memory_id")
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result, err := NewSaveHandler(config.NewLogger(config.LevelError))(ctx, map[string]interface{}{"memories": []interface{}{item}})
			if len(collector.Candidates()) != 0 {
				t.Fatal("invalid item staged")
			}
			if err == nil && !strings.Contains(result.Content, `"status":"rejected"`) {
				t.Fatalf("no rejection: %s", result.Content)
			}
			if kind == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

func TestSaveHandlerAssessmentLogsSafeCounts(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, rejected := range []bool{false, true} {
			var logs bytes.Buffer
			log := config.NewLogger(level)
			log.SetOutput(&logs)
			ctx := requestctx.WithMemoryStageCollector(requestctx.WithMetadata(principalContext("user", "externalcanary"), requestctx.Metadata{RequestID: "req_save", CurrentUserText: "I want privatecanary now."}), requestctx.NewMemoryStageCollector())
			item := map[string]interface{}{"statement": "The user wants privatecanary now.", "evidence": "I want privatecanary now.", "category": "notes", "claim_slot": "notes.desire", "claim_value": "privatecanary", "supersedes": "", "evidence_type": "direct_statement", "confidence": .9, "retention": "observation", "context": "contextcanary"}
			if rejected {
				item["claim_slot"] = "identity.desire"
			}
			_, err := NewSaveHandler(log)(ctx, map[string]interface{}{"memories": []interface{}{item}})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(logs.String(), "\n") != 1 {
				t.Fatalf("unexpected log count: %s", logs.String())
			}
			for _, canary := range []string{"privatecanary", "contextcanary", "externalcanary"} {
				if strings.Contains(logs.String(), canary) {
					t.Fatal("private log content")
				}
			}
			var record map[string]interface{}
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			wantCount := float64(1)
			if rejected {
				wantCount = 0
			}
			if record["event"] != "agent.tool.user_memory.staged" || record["level"] != "info" || record["request_id"] != "req_save" || record["staged_count"] != wantCount || record["rejected_count"] != 1-wantCount {
				t.Fatalf("invalid measurement: %+v", record)
			}
		}
	}
}
