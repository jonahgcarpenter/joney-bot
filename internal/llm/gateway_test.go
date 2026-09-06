package llm

import (
	"encoding/json"
	"testing"
)

func TestMapToGatewayMessagesSerializesImagesAsDataURLs(t *testing.T) {
	messages := mapToGatewayMessages([]ChatMessage{
		{
			Role:    "user",
			Content: "Analyze this image and describe what you see.",
			Images: []InputImage{
				{MimeType: "image/jpeg", Data: "abc123"},
			},
		},
	})

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	raw, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatalf("marshal gateway message: %v", err)
	}

	var got struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal gateway message: %v", err)
	}

	if got.Role != "user" {
		t.Fatalf("expected role user, got %q", got.Role)
	}
	if len(got.Content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(got.Content))
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "Analyze this image and describe what you see." {
		t.Fatalf("unexpected text part: %+v", got.Content[0])
	}
	if got.Content[1].Type != "image_url" || got.Content[1].ImageURL == nil {
		t.Fatalf("unexpected image part: %+v", got.Content[1])
	}
	if got.Content[1].ImageURL.URL != "data:image/jpeg;base64,abc123" {
		t.Fatalf("unexpected image URL %q", got.Content[1].ImageURL.URL)
	}
}

func TestMapFromGatewayMessageDecodesToolArgumentsAndThinking(t *testing.T) {
	msg := mapFromGatewayMessage(gatewayMessage{
		Role:             "assistant",
		Content:          []interface{}{map[string]interface{}{"type": "text", "text": "hello"}, map[string]interface{}{"type": "image_url"}},
		ReasoningContent: "reasoning",
		Thinking:         "thinking",
		ToolCalls: []gatewayToolCall{{ID: "call-1", Function: gatewayToolFunction{
			Name:      "test.tool",
			Arguments: `{"value":42}`,
		}}},
	})

	if msg.Content != "hello" || msg.Thinking != "reasoning" {
		t.Fatalf("unexpected message content/thinking: %+v", msg)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "test.tool" || msg.ToolCalls[0].Function.Arguments["value"].(float64) != 42 || msg.ToolCalls[0].Function.RawArguments != `{"value":42}` {
		t.Fatalf("unexpected tool calls: %+v", msg.ToolCalls)
	}
}

func TestMapFromGatewayMessageUsesReasoningFallback(t *testing.T) {
	msg := mapFromGatewayMessage(gatewayMessage{
		Role:      "assistant",
		Content:   "hello",
		Reasoning: "reasoning",
	})

	if msg.Thinking != "reasoning" {
		t.Fatalf("expected reasoning fallback, got %+v", msg)
	}
}

func TestDecodeToolArgumentsFallsBackToRaw(t *testing.T) {
	got := decodeToolArguments("not-json")
	if got["_raw"] != "not-json" {
		t.Fatalf("unexpected decoded args: %+v", got)
	}
	if responseFormat("json_object").Type != "json_object" {
		t.Fatal("expected canonical json_object response format")
	}
}
