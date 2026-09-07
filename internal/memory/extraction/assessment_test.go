package extraction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func assessmentCandidate() memory.ForegroundMemoryCandidate {
	return memory.ForegroundMemoryCandidate{Statement: "The user uses Go.", Evidence: "I use Go", Category: "projects", ClaimSlot: "project.language", ClaimValue: "go", EvidenceType: "direct_statement", Provenance: "user_statement", Confidence: .95, Retention: "durable", Intent: "automatic", Context: "direct_assertion", Cardinality: "multiple"}
}

type assessmentChatter struct {
	t       *testing.T
	request llm.ChatRequest
	items   []memory.ForegroundMemoryCandidate
	calls   int
}

func (c *assessmentChatter) Chat(_ context.Context, req llm.ChatRequest, callback func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	c.t.Helper()
	if callback != nil {
		c.t.Fatal("private assessment must have no visible callback")
	}
	c.request = req
	c.calls++
	return &llm.ChatResponse{Message: llm.ChatMessage{ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: assessmentToolName, Arguments: map[string]interface{}{"items": c.items}}}}}}, nil
}

func TestAssessmentFirstTurnAndSkippedForegroundRecovery(t *testing.T) {
	for _, empty := range []bool{false, true} {
		items := []memory.ForegroundMemoryCandidate{}
		if !empty {
			items = append(items, assessmentCandidate())
		}
		client := &assessmentChatter{t: t, items: items}
		e := newTestExtractor(t, client)
		batch, err := e.ExtractAssessment(context.Background(), memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 1, UserText: "Remember this: I use Go for work."}}, "")
		if err != nil || len(batch.Items) != len(items) {
			t.Fatalf("batch=%+v err=%v", batch, err)
		}
		r := client.request
		if !r.Stream || r.MaxTokens != 4096 || r.ToolChoice != llm.ToolChoiceRequired || r.ParallelToolCalls == nil || *r.ParallelToolCalls || r.Temperature == nil || *r.Temperature != 0 {
			t.Fatalf("invalid transport controls: %+v", r)
		}
		if len(r.Tools) != 1 || r.Tools[0].Function.Name != assessmentToolName {
			t.Fatal("wrong private tool")
		}
		if !strings.Contains(r.Messages[0].Content, "foreground saving was skipped") {
			t.Fatal("missing recovery instruction")
		}
	}
}

func TestAssessmentRejectsEvidenceAndIDsOutsideActualProjection(t *testing.T) {
	for _, kind := range []string{"anchor span", "observation ID", "memory ID"} {
		t.Run(kind, func(t *testing.T) {
			item := assessmentCandidate()
			input := memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 100, UserText: "I use Go"}}
			switch kind {
			case "anchor span":
				input.Anchor.UserText = strings.Repeat("x", 5000) + " I use Go"
			case "observation ID":
				for i := int64(1); i <= 7; i++ {
					input.Observations = append(input.Observations, memory.MemoryObservation{ID: i, SourceTurnID: i, Statement: "Temporary Go task", Evidence: "I use Go today", ClaimSlot: "notes.task", ClaimValue: "today"})
				}
				item.SourceObservationIDs = []int64{7}
			case "memory ID":
				for i := int64(1); i <= 100; i++ {
					input.Memories = append(input.Memories, memory.AssessmentMemory{ID: i, Revision: 1, ClaimSlot: "project.language", Statement: strings.Repeat("x", 1000)})
				}
				item.TargetMemoryID, item.ExpectedRevision = 100, 1
			}
			payload, err := json.Marshal(memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{item}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeAssessmentBatch(payload, input); err != nil {
				t.Fatalf("fixture must pass full frozen validation: %v", err)
			}
			client := &assessmentChatter{t: t, items: []memory.ForegroundMemoryCandidate{item}}
			e := newTestExtractor(t, client)
			var stats AssessmentMetrics
			reports := 0
			ctx := WithAssessmentMetrics(context.Background(), func(m AssessmentMetrics) { stats = m; reports++ })
			batch, err := e.ExtractAssessment(ctx, input, "")
			if !errors.Is(err, ErrInvalidOutput) || len(batch.Items) != 0 {
				t.Fatalf("omitted source accepted: %+v err=%v", batch, err)
			}
			if reports != 1 || stats.InputTokens <= 0 || client.calls != 1 {
				t.Fatalf("reports=%d stats=%+v", reports, stats)
			}
			switch kind {
			case "anchor span":
				if stats.OmittedTextChars == 0 {
					t.Fatal("missing text omission count")
				}
			case "observation ID":
				if stats.OmittedObservationCount != 1 {
					t.Fatalf("stats=%+v", stats)
				}
			case "memory ID":
				if stats.OmittedMemoryCount == 0 {
					t.Fatal("missing memory omission count")
				}
			}
		})
	}
}

func TestAssessmentProjectionCarriesTrustedTimeAndContextOnlyHints(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.FixedZone("offset", 3600))
	input := memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 3, UserText: "I use Go", CreatedAt: at, AssistantResponse: "anchor-answer-canary"}, Context: []memory.StoredSessionTurn{{ID: 2, UserText: "Yesterday's question", CreatedAt: at.Add(-24 * time.Hour), AssistantResponse: strings.Repeat("a", 1000) + "omitted-prior-answer-canary"}}, Staged: []memory.ForegroundMemoryCandidate{assessmentCandidate()}}
	e := newTestExtractor(t, &assessmentChatter{t: t})
	messages, visible, stats, err := e.assessmentMessages(input, assessmentPolicyPrompt, assessmentTool())
	if err != nil {
		t.Fatal(err)
	}
	payload := messages[1].Content
	for _, want := range []string{`"created_at":"2026-09-01T09:00:00Z"`, `"created_at":"2026-08-31T09:00:00Z"`, `"assistant_response_context_only":`, `"is_context_only":true`, `"staged_foreground_context_only":`, `"is_assistant_context_omitted":true`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("missing context field %s", want)
		}
	}
	if strings.Contains(payload, "anchor-answer-canary") || strings.Contains(payload, "omitted-prior-answer-canary") {
		t.Fatal("included omitted/anchor assistant answer")
	}
	if visible.Anchor.AssistantResponse != "" || len(visible.Context[0].AssistantResponse) != 1000 || stats.OmittedTextChars == 0 {
		t.Fatalf("visible=%+v stats=%+v", visible, stats)
	}
	if input.Anchor.AssistantResponse != "anchor-answer-canary" || !strings.Contains(input.Context[0].AssistantResponse, "omitted-prior-answer-canary") {
		t.Fatal("mutated frozen context")
	}
	for _, evidence := range []string{"Yesterday's question", strings.Repeat("a", 50)} {
		item := assessmentCandidate()
		item.Evidence = evidence
		payload, err := json.Marshal(memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{item}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeAssessmentBatch(payload, visible); !errors.Is(err, ErrInvalidOutput) {
			t.Fatalf("reference-only context accepted as evidence: %v", err)
		}
	}
}

func TestAssessmentInducesPreferenceFromContextualObservationIdentities(t *testing.T) {
	input := memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 3, UserText: "I want pancakes again today."}, Observations: []memory.MemoryObservation{{ID: 11, SourceTurnID: 1, Statement: "The user wants pancakes for breakfast today.", Evidence: "I want pancakes this morning", Context: "temporary breakfast request", ClaimSlot: "notes.breakfast", ClaimValue: "pancakes today"}, {ID: 12, SourceTurnID: 2, Statement: "The user ordered pancakes on Sunday.", Evidence: "I ordered pancakes on Sunday", Context: "historical meal choice", ClaimSlot: "notes.meal", ClaimValue: "Sunday pancakes"}}}
	item := memory.ForegroundMemoryCandidate{Statement: "The user may prefer pancakes for breakfast.", Evidence: input.Anchor.UserText, Category: "durable_preferences", ClaimSlot: "preference.breakfast", ClaimValue: "pancakes", EvidenceType: "model_inference", Provenance: "model_inference", Confidence: .7, Retention: "durable", Intent: "automatic", Cardinality: "multiple", SourceObservationIDs: []int64{11, 12}}
	client := &assessmentChatter{t: t, items: []memory.ForegroundMemoryCandidate{item}}
	batch, err := newTestExtractor(t, client).ExtractAssessment(context.Background(), input, "")
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("meaningful induction rejected: %+v err=%v", batch, err)
	}
	if !strings.Contains(client.request.Messages[0].Content, "claim_slot and claim_value may differ") {
		t.Fatal("missing contextual induction guidance")
	}
}

func TestAssessmentOutputCapUsesResolvedReserve(t *testing.T) {
	for _, reserve := range []int{1024, 4096, 8192} {
		t.Run(fmt.Sprint(reserve), func(t *testing.T) {
			client := &assessmentChatter{t: t, items: []memory.ForegroundMemoryCandidate{}}
			e, err := NewLLMExtractor(client, "model", reserve)
			if err != nil {
				t.Fatal(err)
			}
			e.SetAssessmentBudget(budget.NewContextBudget(32768, reserve))
			if _, err := e.ExtractAssessment(context.Background(), memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 1, UserText: "Hello"}}, ""); err != nil {
				t.Fatal(err)
			}
			if client.request.MaxTokens != min(4096, reserve) {
				t.Fatalf("output cap=%d", client.request.MaxTokens)
			}
		})
	}
}

func TestAssessmentTemporaryAndCrossSessionEvidence(t *testing.T) {
	input := memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 3, SessionID: "session-b", UserText: "I use Go again today."}, Observations: []memory.MemoryObservation{{ID: 21, SourceTurnID: 1, Evidence: "I use Go", ClaimSlot: "project.language", ClaimValue: "go"}}}
	for _, retention := range []string{"durable", "observation"} {
		item := assessmentCandidate()
		item.Retention = retention
		if retention == "observation" {
			item.TTLDays = 7
		}
		item.SourceObservationIDs = []int64{21}
		client := &assessmentChatter{t: t, items: []memory.ForegroundMemoryCandidate{item}}
		batch, err := newTestExtractor(t, client).ExtractAssessment(context.Background(), input, "")
		if err != nil || len(batch.Items) != 1 {
			t.Fatalf("batch=%+v err=%v", batch, err)
		}
		if !strings.Contains(client.request.Messages[1].Content, `"SourceTurnID":1`) {
			t.Fatal("missing independent evidence")
		}
	}
}

func TestAssessmentRejectsUnpublishableWholeBatch(t *testing.T) {
	for name, mutate := range map[string]func(*memory.ForegroundMemoryCandidate){
		"bad evidence":      func(c *memory.ForegroundMemoryCandidate) { c.Evidence = "assistant-only canary" },
		"too long evidence": func(c *memory.ForegroundMemoryCandidate) { c.Evidence = strings.Repeat("x", 1001) },
		"bad provenance":    func(c *memory.ForegroundMemoryCandidate) { c.Provenance = "tool_output" },
		"bad confidence":    func(c *memory.ForegroundMemoryCandidate) { c.Confidence = 1.1 },
		"bad ttl":           func(c *memory.ForegroundMemoryCandidate) { c.TTLDays = 31 },
		"durable ttl":       func(c *memory.ForegroundMemoryCandidate) { c.TTLDays = 2 },
		"bad slot":          func(c *memory.ForegroundMemoryCandidate) { c.ClaimSlot = "identity.name" },
		"bad retention":     func(c *memory.ForegroundMemoryCandidate) { c.Retention = "forever" },
		"bad cardinality":   func(c *memory.ForegroundMemoryCandidate) { c.Cardinality = "many" },
		"unknown source":    func(c *memory.ForegroundMemoryCandidate) { c.SourceObservationIDs = []int64{99} },
		"duplicate source":  func(c *memory.ForegroundMemoryCandidate) { c.SourceObservationIDs = []int64{21, 21} },
		"same turn quotes":  func(c *memory.ForegroundMemoryCandidate) { c.SourceObservationIDs = []int64{21, 22} },
		"unknown target":    func(c *memory.ForegroundMemoryCandidate) { c.TargetMemoryID = 8; c.ExpectedRevision = 1 },
		"stale revision":    func(c *memory.ForegroundMemoryCandidate) { c.TargetMemoryID = 7; c.ExpectedRevision = 1 },
		"retire no target":  func(c *memory.ForegroundMemoryCandidate) { c.Intent = "retire" },
		"inferred correction": func(c *memory.ForegroundMemoryCandidate) {
			c.Intent = "correction"
			c.TargetMemoryID = 7
			c.ExpectedRevision = 2
			c.EvidenceType = "model_inference"
			c.Provenance = "model_inference"
		},
	} {
		t.Run(name, func(t *testing.T) {
			item := assessmentCandidate()
			mutate(&item)
			input := memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 3, UserText: "I use Go " + strings.Repeat("x", 1001)}, Observations: []memory.MemoryObservation{{ID: 21, SourceTurnID: 1}, {ID: 22, SourceTurnID: 1}}, Memories: []memory.AssessmentMemory{{ID: 7, Revision: 2}}}
			payload, err := json.Marshal(memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{assessmentCandidate(), item}})
			if err != nil {
				t.Fatal(err)
			}
			batch, err := DecodeAssessmentBatch(payload, input)
			if !errors.Is(err, ErrInvalidOutput) || len(batch.Items) != 0 {
				t.Fatalf("batch=%+v err=%v", batch, err)
			}
		})
	}
}

func TestAssessmentBoundedInputAndCorrectivePrompt(t *testing.T) {
	client := &assessmentChatter{t: t, items: []memory.ForegroundMemoryCandidate{}}
	e := newTestExtractor(t, client)
	e.SetAssessmentBudget(budget.NewContextBudget(12000, 8192))
	input := memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 1, UserText: strings.Repeat("\u754c", 20000), SessionID: "assistant-only canary"}}
	_, err := e.ExtractAssessment(context.Background(), input, "invalid_assessment")
	if err != nil {
		t.Fatal(err)
	}
	r := client.request
	if n := budget.EstimateRequest(r.Messages, r.Tools); n > min(6000, e.assessmentBudget.UsableInputLimit()) {
		t.Fatalf("request exceeds capacity: %d", n)
	}
	if !strings.Contains(r.Messages[1].Content, `"is_text_omitted":true`) || strings.Contains(r.Messages[1].Content, "assistant-only canary") {
		t.Fatal("unsafe source projection")
	}
	if !strings.Contains(r.Messages[0].Content, "previous assessment failed") {
		t.Fatal("missing reason-aware retry")
	}
	if len([]rune(input.Anchor.UserText)) != 20000 {
		t.Fatal("mutated frozen input")
	}
	e.SetAssessmentBudget(budget.NewContextBudget(8200, 8192))
	if _, err := e.ExtractAssessment(context.Background(), input, ""); !errors.Is(err, ErrPermanentExtraction) {
		t.Fatalf("oversized required context err=%v", err)
	}
}

func TestAssessmentStrictArtifactShape(t *testing.T) {
	for _, payload := range []string{`{"version":1,"items":null}`, `{"version":2,"items":[]}`, `{"version":1,"items":[],"extra":1}`, `{"version":1,"items":[]} {}`} {
		if _, err := DecodeAssessmentBatch([]byte(payload), memory.AssessmentInput{}); !errors.Is(err, ErrInvalidOutput) {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
	}
	items := make([]memory.ForegroundMemoryCandidate, 6)
	for i := range items {
		items[i] = assessmentCandidate()
	}
	payload, err := json.Marshal(memory.AssessmentBatch{Version: 1, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAssessmentBatch(payload, memory.AssessmentInput{Anchor: memory.StoredSessionTurn{UserText: "I use Go"}}); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("oversized batch err=%v", err)
	}
}

func TestAssessmentCurrentCorrectionAndRetirement(t *testing.T) {
	for _, intent := range []string{"correction", "retire"} {
		item := assessmentCandidate()
		item.Intent, item.TargetMemoryID, item.ExpectedRevision = intent, 7, 2
		item.Evidence = "I no longer use Rust; I use Go."
		item.SupersedesStatement = "The user uses Rust."
		if intent == "retire" {
			item.Statement, item.ClaimValue = "The user no longer uses Rust.", "rust"
		}
		input := memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 3, UserText: item.Evidence}, Memories: []memory.AssessmentMemory{{ID: 7, Revision: 2, Statement: item.SupersedesStatement, ClaimSlot: "project.language", ClaimValue: "rust"}}}
		client := &assessmentChatter{t: t, items: []memory.ForegroundMemoryCandidate{item}}
		batch, err := newTestExtractor(t, client).ExtractAssessment(context.Background(), input, "")
		if err != nil || len(batch.Items) != 1 || batch.Items[0].Intent != intent {
			t.Fatalf("batch=%+v err=%v", batch, err)
		}
		if !strings.Contains(client.request.Messages[0].Content, "Do not invent the opposite") {
			t.Fatal("missing correction semantics")
		}
	}
}

func TestAssessmentEllipticalConfirmationUsesOnlyAnchorEvidence(t *testing.T) {
	input := memory.AssessmentInput{Version: 1, Anchor: memory.StoredSessionTurn{ID: 2, UserText: "Yes, I do"}, Context: []memory.StoredSessionTurn{{ID: 1, UserText: "Let's discuss reply style", AssistantResponse: "Do you prefer concise replies?"}}}
	item := assessmentCandidate()
	item.Statement, item.Evidence = "The user prefers concise replies.", input.Anchor.UserText
	item.Category, item.ClaimSlot, item.ClaimValue = "communication_preferences", "communication.reply_style", "concise"
	client := &assessmentChatter{t: t, items: []memory.ForegroundMemoryCandidate{item}}
	batch, err := newTestExtractor(t, client).ExtractAssessment(context.Background(), input, "")
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("confirmed elliptical answer rejected: %+v err=%v", batch, err)
	}
	if !strings.Contains(client.request.Messages[0].Content, "first-person and elliptical answers") {
		t.Fatal("missing elliptical-context policy")
	}
	client.items[0].Evidence = input.Context[0].AssistantResponse
	if _, err := newTestExtractor(t, client).ExtractAssessment(context.Background(), input, ""); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("assistant question accepted as evidence: %v", err)
	}
}
