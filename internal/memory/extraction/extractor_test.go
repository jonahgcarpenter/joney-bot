package extraction

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func TestUserMemorySaveToolMatchesBatchContract(t *testing.T) {
	tool := userMemorySaveTool()
	memories := tool.Function.Parameters.Properties["memories"]
	if tool.Function.Name != userMemorySaveToolName || memories.Items == nil || memories.MaxItems == nil || *memories.MaxItems != maxExtractedMemoryBatch {
		t.Fatalf("private tool schema=%+v", tool)
	}
	if memories.Items.AdditionalProperties == nil || *memories.Items.AdditionalProperties || len(memories.Items.Required) != 13 {
		t.Fatalf("private memory item schema=%+v", memories.Items)
	}
	importance := memories.Items.Properties["importance"]
	if importance.Minimum == nil || importance.Maximum == nil || *importance.Minimum != 1 || *importance.Maximum != 5 {
		t.Fatalf("private importance schema=%+v", importance)
	}
	for field, bounds := range map[string][2]float64{"confidence": {0, 1}, "ttl_days": {0, 30}} {
		property := memories.Items.Properties[field]
		if property.Minimum == nil || property.Maximum == nil || *property.Minimum != bounds[0] || *property.Maximum != bounds[1] {
			t.Fatalf("private %s schema=%+v", field, property)
		}
	}
	for _, field := range memory.MemorySaveRequiredFields() {
		if _, ok := memories.Items.Properties[field]; !ok {
			t.Fatalf("private schema missing %q", field)
		}
	}
}

func TestNewLLMExtractorValidatesDependencies(t *testing.T) {
	if _, err := NewLLMExtractor(nil, "model", 8192); err == nil {
		t.Fatal("expected missing client error")
	}
	if _, err := NewLLMExtractor(&fakeChatter{}, " ", 8192); err == nil {
		t.Fatal("expected missing model error")
	}
	if _, err := NewLLMExtractor(&fakeChatter{}, "model", 0); err == nil {
		t.Fatal("expected invalid max output tokens error")
	}
}

func TestLLMExtractorParsesStrictJSON(t *testing.T) {
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{validCandidate()}}}
	extractor := newTestExtractor(t, client)
	got, err := extractor.Extract(context.Background(), memory.StoredSessionTurn{UserText: "I use Go"}, "")
	if err != nil || len(got.Memories) != 1 || got.Memories[0].Evidence != "I use Go" || got.Memories[0].ClaimSlot != "project.language" {
		t.Fatalf("extracted=%+v err=%v", got, err)
	}
	if len(client.request.Tools) != 1 || client.request.Tools[0].Function.Name != userMemorySaveToolName || client.request.ToolChoice != llm.ToolChoiceRequired || client.request.ParallelToolCalls == nil || *client.request.ParallelToolCalls {
		t.Fatalf("request did not force the private memory save tool: %+v", client.request)
	}
	if client.request.Temperature == nil || *client.request.Temperature != 0 || client.request.MaxTokens != 8192 || !client.request.Stream {
		t.Fatalf("request did not apply deterministic extraction controls: %+v", client.request)
	}
	for _, required := range []string{"all 13 fields", "Evidence must be the complete user turn", "identity -> identity.*", "identity.name, never identity_name", "topic mention does not establish", "importance must be an integer from 1 to 5", `"category":"communication_preferences"`, `"claim_slot":"communication.reply_style"`, `{"memories":[]}`} {
		if !strings.Contains(client.request.Messages[0].Content, required) {
			t.Fatalf("extractor policy prompt missing %q", required)
		}
	}
}

func TestLLMExtractorRejectsAllMalformedCandidates(t *testing.T) {
	malformed := validCandidate()
	delete(malformed, "claim_slot")
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{malformed}}}
	got, err := newTestExtractor(t, client).Extract(context.Background(), memory.StoredSessionTurn{UserText: "I use Go"}, "")
	code, ok := InvalidOutputCode(err)
	if !errors.Is(err, ErrInvalidOutput) || errors.Is(err, ErrPermanentExtraction) || !ok || code != invalidOutputAllItemsMalformed || len(got.Memories) != 0 || client.calls != 1 {
		t.Fatalf("extracted=%+v calls=%d err=%v", got, client.calls, err)
	}
}

func TestLLMExtractorRejectsCandidateMissingCategory(t *testing.T) {
	malformed := validCandidate()
	delete(malformed, "category")
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{malformed}}}
	_, err := newTestExtractor(t, client).Extract(context.Background(), memory.StoredSessionTurn{UserText: "My name is Jonah"}, "")
	if code, ok := InvalidOutputCode(err); !errors.Is(err, ErrInvalidOutput) || !ok || code != invalidOutputAllItemsMalformed {
		t.Fatalf("error=%v", err)
	}
}

func TestLLMExtractorPreservesValidCandidatesBesideMalformedCandidates(t *testing.T) {
	malformed := validCandidate()
	delete(malformed, "category")
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{malformed, validCandidate()}}}
	got, err := newTestExtractor(t, client).Extract(context.Background(), memory.StoredSessionTurn{UserText: "I use Go"}, "")
	if err != nil || len(got.Memories) != 1 || got.Memories[0].Statement != "The user uses Go." || got.SubmittedCount != 2 || got.MalformedCount != 1 {
		t.Fatalf("extracted=%+v err=%v", got, err)
	}
}

func TestLLMExtractorRejectsMoreThanFiveCandidates(t *testing.T) {
	items := make([]interface{}, maxExtractedMemoryBatch+1)
	for i := range items {
		items[i] = validCandidate()
	}
	client := &fakeChatter{arguments: map[string]interface{}{"memories": items}}
	_, err := newTestExtractor(t, client).Extract(context.Background(), memory.StoredSessionTurn{UserText: "I use several tools"}, "")
	if code, ok := InvalidOutputCode(err); !errors.Is(err, ErrInvalidOutput) || !ok || code != invalidOutputBatchShape {
		t.Fatalf("error=%v", err)
	}
}

func TestLLMExtractorReturnsStableStructuralFailureCodes(t *testing.T) {
	tests := []struct {
		name     string
		response *llm.ChatResponse
		wantCode string
	}{
		{name: "missing tool call", response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant"}}, wantCode: invalidOutputMissingToolCall},
		{name: "multiple tool calls", response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: userMemorySaveToolName}}, {Function: llm.ToolFunction{Name: userMemorySaveToolName}}}}}, wantCode: invalidOutputMultipleToolCalls},
		{name: "unexpected tool", response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: "other_tool"}}}}}, wantCode: invalidOutputUnexpectedToolCall},
		{name: "malformed arguments", response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: userMemorySaveToolName, Arguments: map[string]interface{}{"_raw": "not json"}}}}}}, wantCode: invalidOutputMalformedArguments},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newTestExtractor(t, &fakeChatter{response: test.response}).Extract(context.Background(), memory.StoredSessionTurn{UserText: "I use Go"}, "")
			if code, ok := InvalidOutputCode(err); !errors.Is(err, ErrInvalidOutput) || !ok || code != test.wantCode {
				t.Fatalf("code=%q error=%v", code, err)
			}
		})
	}
}

func TestLLMExtractorClassifiesProviderErrors(t *testing.T) {
	permanent := &fakeChatter{err: &llm.ChatHTTPError{StatusCode: 400, Body: "unsupported response format"}}
	_, err := newTestExtractor(t, permanent).Extract(context.Background(), memory.StoredSessionTurn{UserText: "I use Go"}, "")
	if !errors.Is(err, ErrPermanentExtraction) {
		t.Fatalf("permanent error=%v", err)
	}

	transient := &fakeChatter{err: &llm.ChatHTTPError{StatusCode: 503, Body: "unavailable"}}
	_, err = newTestExtractor(t, transient).Extract(context.Background(), memory.StoredSessionTurn{UserText: "I use Go"}, "")
	if err == nil || errors.Is(err, ErrPermanentExtraction) {
		t.Fatalf("transient error=%v", err)
	}
}

func TestLLMExtractorUsesSeparateStrictPatternToolAndUserTurnsOnly(t *testing.T) {
	arguments := map[string]interface{}{"patterns": []interface{}{map[string]interface{}{
		"statement": "The user may favor concise review communication.", "category": "communication_preferences",
		"claim_slot": "communication.review_style", "claim_value": "concise", "sensitivity": "low", "confidence": 0.72,
		"observations": []interface{}{
			map[string]interface{}{"source_turn_id": 1, "evidence": "I keep review notes concise."},
			map[string]interface{}{"source_turn_id": 2, "evidence": "I repeatedly write concise reviews."},
		},
	}}}
	client := &fakeChatter{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: userMemoryPatternToolName, Arguments: arguments}}}}}}
	extractor := newTestExtractor(t, client)
	batch, err := extractor.ExtractPatterns(context.Background(), []memory.StoredSessionTurn{{ID: 1, UserText: "I keep review notes concise."}, {ID: 2, UserText: "I repeatedly write concise reviews."}}, "")
	if err != nil || len(batch.Patterns) != 1 || len(batch.Patterns[0].Observations) != 2 {
		t.Fatalf("batch=%+v err=%v", batch, err)
	}
	if len(client.request.Tools) != 1 || client.request.Tools[0].Function.Name != userMemoryPatternToolName || client.request.ToolChoice != llm.ToolChoiceRequired || !client.request.Stream {
		t.Fatalf("pattern request=%+v", client.request)
	}
	patternSchema := client.request.Tools[0].Function.Parameters.Properties["patterns"].Items
	if patternSchema == nil || patternSchema.Properties["confidence"].Minimum == nil || patternSchema.Properties["confidence"].Maximum == nil || !slices.Contains(patternSchema.Required, "confidence") {
		t.Fatalf("pattern confidence schema=%+v", patternSchema)
	}
	if !strings.Contains(patternSchema.Properties["claim_slot"].Description, "project.*") || !strings.Contains(patternSchema.Properties["observations"].Items.Properties["evidence"].Description, "Exact complete user_text") {
		t.Fatalf("pattern schema omitted semantic constraints: %+v", patternSchema)
	}
	if !strings.Contains(client.request.Messages[0].Content, "holistic assessment") || !strings.Contains(client.request.Messages[0].Content, "not confidence per observation") {
		t.Fatalf("pattern confidence prompt=%q", client.request.Messages[0].Content)
	}
	if !strings.Contains(client.request.Messages[0].Content, "projects -> project.*") || !strings.Contains(client.request.Messages[0].Content, "never cassette_recording_project or tone_style") {
		t.Fatalf("pattern claim identity prompt=%q", client.request.Messages[0].Content)
	}
	userPayload := client.request.Messages[1].Content
	if !strings.Contains(userPayload, `"source_turn_id":1`) || strings.Contains(userPayload, "assistant") || strings.Contains(userPayload, "tool") {
		t.Fatalf("unexpected private pattern input %q", userPayload)
	}
}

func TestLLMExtractorAddsReasonAwareStructuredRetryInstructions(t *testing.T) {
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{}}}
	if _, err := newTestExtractor(t, client).Extract(context.Background(), memory.StoredSessionTurn{UserText: "Nothing durable"}, invalidOutputMissingToolCall); err != nil {
		t.Fatal(err)
	}
	prompt := client.request.Messages[0].Content
	for _, expected := range []string{"STRUCTURED OUTPUT RETRY", "omitted the required tool call", "call user_memory_save exactly once", `{"memories":[]}`} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("corrective prompt missing %q: %q", expected, prompt)
		}
	}
	if instruction := structuredRetryInstruction("transient_provider_error", userMemorySaveToolName, `{"memories":[]}`); instruction != "" {
		t.Fatalf("operational failure produced corrective instruction %q", instruction)
	}

	patternClient := &fakeChatter{response: &llm.ChatResponse{Message: llm.ChatMessage{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{
			Name:      userMemoryPatternToolName,
			Arguments: map[string]interface{}{"patterns": []interface{}{}},
		}}},
	}}}
	if _, err := newTestExtractor(t, patternClient).ExtractPatterns(context.Background(), []memory.StoredSessionTurn{{ID: 1, UserText: "one"}, {ID: 2, UserText: "two"}}, invalidOutputBatchShape); err != nil {
		t.Fatal(err)
	}
	patternPrompt := patternClient.request.Messages[0].Content
	if !strings.Contains(patternPrompt, "arguments did not satisfy") || !strings.Contains(patternPrompt, `{"patterns":[]}`) {
		t.Fatalf("pattern corrective prompt=%q", patternPrompt)
	}
	for _, test := range []struct {
		code string
		want string
	}{
		{invalidOutputPatternClaimSlot, "did not belong to its category"},
		{invalidOutputPatternSource, "was not in the supplied frozen window"},
		{invalidOutputPatternEvidence, "did not exactly equal the complete user_text"},
		{invalidOutputPatternAnchor, "did not include the newest final supplied turn"},
	} {
		if instruction := structuredRetryInstruction(test.code, userMemoryPatternToolName, `{"patterns":[]}`); !strings.Contains(instruction, test.want) {
			t.Fatalf("retry code=%q instruction=%q", test.code, instruction)
		}
	}
}

func TestLLMExtractorRejectsSemanticallyInvalidPatternBatchBeforePersistence(t *testing.T) {
	basePattern := func() map[string]interface{} {
		return map[string]interface{}{
			"statement": "The user may prefer concise notes.", "category": "communication_preferences",
			"claim_slot": "communication.note_style", "claim_value": "concise", "sensitivity": "low", "confidence": 0.7,
			"observations": []interface{}{map[string]interface{}{"source_turn_id": 1, "evidence": "one"}, map[string]interface{}{"source_turn_id": 2, "evidence": "two"}},
		}
	}
	tests := []struct {
		name     string
		turns    []memory.StoredSessionTurn
		mutate   func(map[string]interface{})
		wantCode string
	}{
		{name: "category claim slot", turns: []memory.StoredSessionTurn{{ID: 1, UserText: "one"}, {ID: 2, UserText: "two"}}, mutate: func(pattern map[string]interface{}) { pattern["claim_slot"] = "tone_style" }, wantCode: invalidOutputPatternClaimSlot},
		{name: "unknown source", turns: []memory.StoredSessionTurn{{ID: 1, UserText: "one"}, {ID: 2, UserText: "two"}}, mutate: func(pattern map[string]interface{}) {
			pattern["observations"].([]interface{})[0].(map[string]interface{})["source_turn_id"] = 99
		}, wantCode: invalidOutputPatternSource},
		{name: "rewritten evidence", turns: []memory.StoredSessionTurn{{ID: 1, UserText: "one"}, {ID: 2, UserText: "two"}}, mutate: func(pattern map[string]interface{}) {
			pattern["observations"].([]interface{})[0].(map[string]interface{})["evidence"] = "rewritten"
		}, wantCode: invalidOutputPatternEvidence},
		{name: "missing newest anchor", turns: []memory.StoredSessionTurn{{ID: 1, UserText: "one"}, {ID: 2, UserText: "two"}, {ID: 3, UserText: "three"}}, mutate: func(map[string]interface{}) {}, wantCode: invalidOutputPatternAnchor},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pattern := basePattern()
			test.mutate(pattern)
			arguments := map[string]interface{}{"patterns": []interface{}{pattern}}
			client := &fakeChatter{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: userMemoryPatternToolName, Arguments: arguments}}}}}}
			batch, err := newTestExtractor(t, client).ExtractPatterns(context.Background(), test.turns, "")
			if code, ok := InvalidOutputCode(err); !ok || code != test.wantCode || len(batch.Patterns) != 0 {
				t.Fatalf("batch=%+v code=%q err=%v", batch, code, err)
			}
		})
	}
}

func TestLLMExtractorRejectsMalformedAndSpoofablePatternShapes(t *testing.T) {
	for _, arguments := range []map[string]interface{}{
		{"patterns": []interface{}{map[string]interface{}{"statement": "The user may prefer concise replies."}}},
		{"patterns": []interface{}{map[string]interface{}{"statement": "The user may prefer concise replies.", "category": "communication_preferences", "claim_slot": "communication.reply_style", "claim_value": "concise", "sensitivity": "low", "confidence": 0.6, "observations": []interface{}{map[string]interface{}{"source_turn_id": 1, "evidence": "one"}, map[string]interface{}{"source_turn_id": 1, "evidence": "one"}}}}},
	} {
		client := &fakeChatter{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: userMemoryPatternToolName, Arguments: arguments}}}}}}
		_, err := newTestExtractor(t, client).ExtractPatterns(context.Background(), []memory.StoredSessionTurn{{ID: 1, UserText: "one"}, {ID: 2, UserText: "two"}}, "")
		if code, ok := InvalidOutputCode(err); !ok || code != invalidOutputBatchShape {
			t.Fatalf("arguments=%#v error=%v", arguments, err)
		}
	}
}

type fakeChatter struct {
	arguments map[string]interface{}
	response  *llm.ChatResponse
	err       error
	request   llm.ChatRequest
	calls     int
}

func (f *fakeChatter) Chat(_ context.Context, request llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.calls++
	f.request = request
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return f.response, nil
	}
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: userMemorySaveToolName, Arguments: f.arguments}}}}}, nil
}

func newTestExtractor(t *testing.T, client llm.Chatter) *LLMExtractor {
	t.Helper()
	extractor, err := NewLLMExtractor(client, "model", 8192)
	if err != nil {
		t.Fatal(err)
	}
	return extractor
}

func validCandidate() map[string]interface{} {
	return map[string]interface{}{
		"statement": "The user uses Go.", "evidence": "I use Go", "scope": "long_term", "category": "projects",
		"context": "direct_assertion", "provenance": "user_statement", "sensitivity": "low", "confidence": 0.9,
		"importance": 4, "ttl_days": 0, "supersedes": "", "claim_slot": "project.language", "claim_value": "go",
	}
}
