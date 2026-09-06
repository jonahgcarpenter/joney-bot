package imessage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/runtimeinvalidation"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/testutil"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestRuntimeInvalidationPurgesOnlyMatchingIMessageState(t *testing.T) {
	g := &Gateway{
		messageIndex: map[string]messageContext{
			"session": {SessionKey: "imessage:chat:one", SenderID: "+15550000001"},
			"sender":  {SessionKey: "imessage:other:one", SenderID: "+15550000001"},
			"foreign": {SessionKey: "imessage:chat:two", SenderID: "+15550000002"},
		},
		contactNames: map[string]contactNameCacheEntry{
			"+15550000001": {DisplayName: "One"},
			"+15550000002": {DisplayName: "Two"},
		},
	}
	g.HandleRuntimeInvalidation(runtimeinvalidation.Event{SessionIDs: []string{"imessage:chat:one"}, ExternalIdentities: []string{"imessage:+15550000001", "discord:one"}})
	if _, ok := g.messageIndex["session"]; ok {
		t.Fatal("matching session message context remained")
	}
	if _, ok := g.messageIndex["sender"]; ok {
		t.Fatal("matching sender message context remained")
	}
	if _, ok := g.messageIndex["foreign"]; !ok || len(g.messageIndex) != 1 {
		t.Fatalf("foreign message context was purged: %+v", g.messageIndex)
	}
	if _, ok := g.contactNames["+15550000001"]; ok {
		t.Fatal("matching contact cache entry remained")
	}
	if _, ok := g.contactNames["+15550000002"]; !ok || len(g.contactNames) != 1 {
		t.Fatalf("foreign contact cache entry was purged: %+v", g.contactNames)
	}
}

func TestIMessageProcessDirectMessageSendsReply(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "hello imessage",
		Handle: messageHandle{Address: "+1 (555) 123-4567"},
		Chats:  []messageChat{{GUID: "chat-direct", Style: chatStyleDirect}},
	})

	primary := primaryIMessageRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	last := primary[0].Messages[len(primary[0].Messages)-1]
	if last.Content != "hello imessage" || !strings.Contains(primary[0].Messages[len(primary[0].Messages)-2].Content, "<tenant_profile") {
		t.Fatalf("unexpected prompt %q", last.Content)
	}
	if bb.sentMessage() != "imessage response" {
		t.Fatalf("unexpected sent messages: %+v", bb.sentMessages())
	}
	if sent := bb.sentMessages(); len(sent) != 1 || sent[0].Method != "" || sent[0].SelectedMessageGUID != "" {
		t.Fatalf("expected plain text send, got %+v", sent)
	}
	if _, ok := g.lookupMessage("sent-1"); !ok {
		t.Fatal("expected sent bot message remembered")
	}
	principal := chat.lastPrincipal()
	if principal.CanonicalUserID == "" || principal.Gateway != "imessage" || principal.ExternalID != "+15551234567" || principal.Assurance != identity.AssuranceBlueBubblesWebhook {
		t.Fatalf("unexpected principal: %+v", principal)
	}
}

func TestIMessageCommandAttachmentMultipartSend(t *testing.T) {
	var filename, mimeType, chatGUID, name, tempGUID string
	var attachmentData []byte
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.URL.Query().Get("password") != "pw" {
			t.Fatalf("missing BlueBubbles password")
		}
		if r.URL.Path != "/api/v1/message/attachment" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(commands.MaxAttachmentBytes + 1024); err != nil {
			t.Fatalf("parse attachment: %v", err)
		}
		file, header, err := r.FormFile("attachment")
		if err != nil {
			t.Fatalf("read attachment: %v", err)
		}
		defer file.Close()
		filename = header.Filename
		mimeType = header.Header.Get("Content-Type")
		attachmentData, err = io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		chatGUID = r.FormValue("chatGuid")
		name = r.FormValue("name")
		tempGUID = r.FormValue("tempGuid")
		_, _ = w.Write([]byte(`{"data":{"guid":"sent-1"}}`))
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	err := g.sendCommandAttachment("chat-1", commands.Attachment{Filename: "export.json", MIMEType: "application/json", Data: []byte("private-content")})
	if err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 || filename != "export.json" || mimeType != "application/json" || string(attachmentData) != "private-content" || chatGUID != "chat-1" || name != "export.json" || tempGUID == "" {
		t.Fatalf("unexpected attachment request count=%d filename=%q mime=%q data=%q chat=%q name=%q temp_set=%t", requestCount, filename, mimeType, attachmentData, chatGUID, name, tempGUID != "")
	}
}

func TestIMessageCommandAttachmentProviderFailures(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "private-content", http.StatusBadGateway)
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	err := g.sendCommandAttachment("chat-1", commands.Attachment{Filename: "export.json", MIMEType: "application/json", Data: []byte("private-content")})
	if err == nil || strings.Contains(err.Error(), "private-content") || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestIMessageCommandAttachmentsSendPartsBeforeSuccessAndStopOnFailure(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/message/text":
			calls = append(calls, "text")
			_, _ = w.Write([]byte(`{"data":{"guid":"text-1"}}`))
		case "/api/v1/message/attachment":
			if err := r.ParseMultipartForm(commands.MaxAttachmentBytes + 1024); err != nil {
				t.Fatal(err)
			}
			file, header, err := r.FormFile("attachment")
			if err != nil {
				t.Fatal(err)
			}
			_ = file.Close()
			calls = append(calls, "send:"+header.Filename)
			if header.Filename == "part002" {
				http.Error(w, "failed", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"guid":"attachment-1"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext)}
	responder := runtimeResponder{gateway: g, chatGUID: "chat-1"}
	err := responder.SendCommandResponse(commands.Result{Text: "join in order", Attachments: []commands.Attachment{
		{Filename: "part001", MIMEType: "application/octet-stream", Data: []byte("first")},
		{Filename: "part002", MIMEType: "application/octet-stream", Data: []byte("second")},
		{Filename: "part003", MIMEType: "application/octet-stream", Data: []byte("third")},
	}})
	if err == nil {
		t.Fatal("second-part failure was ignored")
	}
	want := []string{"send:part001", "send:part002"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want %v", calls, want)
	}
}

func TestIMessageAgentResponseDeliversAttachmentBeforeText(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/message/attachment":
			if err := r.ParseMultipartForm(commands.MaxAttachmentBytes + 4096); err != nil {
				t.Fatal(err)
			}
			_, header, err := r.FormFile("attachment")
			if err != nil {
				t.Fatal(err)
			}
			calls = append(calls, "attachment:"+header.Filename)
			_, _ = w.Write([]byte(`{"data":{"guid":"attachment-guid"}}`))
		case "/api/v1/message/text":
			calls = append(calls, "text")
			_, _ = w.Write([]byte(`{"data":{"guid":"text-guid"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext)}
	responder := runtimeResponder{gateway: g, chatGUID: "chat-1", requestID: "request"}
	err := responder.SendAgentResponse(&agent.AgentResponse{Response: "generated", Attachments: []media.OutputAttachment{{Filename: "generated.png", MIMEType: "image/png", Data: []byte("image-data")}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"attachment:generated.png", "text"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestIMessageWebhookRequiresBlueBubblesCredential(t *testing.T) {
	g := &Gateway{BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	req := httptest.NewRequest(http.MethodPost, "/bluebubbles/webhook", strings.NewReader(`{"type":"typing-indicator"}`))
	rec := httptest.NewRecorder()

	g.handleWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", rec.Code)
	}
}

func TestIMessageWebhookAcceptsPasswordCredential(t *testing.T) {
	g := &Gateway{BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	req := httptest.NewRequest(http.MethodPost, "/bluebubbles/webhook?password=pw", strings.NewReader(`{"type":"typing-indicator"}`))
	rec := httptest.NewRecorder()

	g.handleWebhook(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected no content, got %d", rec.Code)
	}
}

func TestIMessageWebhookAcceptsCredentialSources(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		header string
	}{
		{name: "password query", path: "/bluebubbles/webhook?password=pw"},
		{name: "guid query", path: "/bluebubbles/webhook?guid=pw"},
		{name: "x-password header", path: "/bluebubbles/webhook", header: "x-password"},
		{name: "x-guid header", path: "/bluebubbles/webhook", header: "x-guid"},
		{name: "x-bluebubbles-guid header", path: "/bluebubbles/webhook", header: "x-bluebubbles-guid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &Gateway{BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(`{"type":"typing-indicator"}`))
			if tt.header != "" {
				req.Header.Set(tt.header, "pw")
			}
			rec := httptest.NewRecorder()

			g.handleWebhook(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected no content, got %d", rec.Code)
			}
		})
	}
}

func TestIMessageWebhookRejectsWrongCredential(t *testing.T) {
	g := &Gateway{BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	req := httptest.NewRequest(http.MethodPost, "/bluebubbles/webhook?password=wrong", strings.NewReader(`{"type":"typing-indicator"}`))
	rec := httptest.NewRecorder()

	g.handleWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", rec.Code)
	}
}

func TestIMessageWebhookMalformedJSONReturnsBadRequest(t *testing.T) {
	g := &Gateway{BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	req := httptest.NewRequest(http.MethodPost, "/bluebubbles/webhook?password=pw", strings.NewReader(`{"type":`))
	rec := httptest.NewRecorder()

	g.handleWebhook(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d", rec.Code)
	}
}

func TestIMessageWebhookWrongMethodReturnsMethodNotAllowed(t *testing.T) {
	g := &Gateway{BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	req := httptest.NewRequest(http.MethodGet, "/bluebubbles/webhook?password=pw", nil)
	rec := httptest.NewRecorder()

	g.handleWebhook(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected method not allowed, got %d", rec.Code)
	}
}

func TestIMessageWebhookDirectMessageRoutesAndReplies(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	rec := postIMessageWebhook(t, g, `{"type":"new-message","data":{"guid":"msg-1","text":"hello from webhook","isFromMe":false,"handle":{"address":"+15551234567"},"chats":[{"guid":"chat-direct","style":45}]}}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected accepted, got %d", rec.Code)
	}
	primary := waitForPrimaryIMessageRequests(t, chat, 1)
	last := primary[0].Messages[len(primary[0].Messages)-1]
	if last.Content != "hello from webhook" || !strings.Contains(primary[0].Messages[len(primary[0].Messages)-2].Content, "<tenant_profile") {
		t.Fatalf("unexpected prompt %q", last.Content)
	}
	if !bb.waitForSentCount(1) {
		t.Fatalf("expected reply send, got %+v", bb.sentMessages())
	}
	if bb.sentMessage() != "imessage response" {
		t.Fatalf("unexpected sent message %q", bb.sentMessage())
	}
}

func TestIMessageWebhookGroupMentionRoutesCleanedText(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	rec := postIMessageWebhook(t, g, `{"type":"new-message","data":{"guid":"msg-1","text":"@Oswald hello","isFromMe":false,"handle":{"address":"+15551234567"},"chats":[{"guid":"chat;+;group","style":43}]}}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected accepted, got %d", rec.Code)
	}
	primary := waitForPrimaryIMessageRequests(t, chat, 1)
	last := primary[0].Messages[len(primary[0].Messages)-1]
	if last.Content != "hello" || !strings.Contains(primary[0].Messages[len(primary[0].Messages)-2].Content, "<tenant_profile") {
		t.Fatalf("unexpected prompt %q", last.Content)
	}
}

func TestIMessageWebhookGroupWithoutMentionSkips(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	rec := postIMessageWebhook(t, g, `{"type":"new-message","data":{"guid":"msg-1","text":"casual group chatter","isFromMe":false,"handle":{"address":"+15551234567"},"chats":[{"guid":"chat;+;group","style":43}]}}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected accepted, got %d", rec.Code)
	}
	if waitForPrimaryIMessageRequestCount(chat, 1, 100*time.Millisecond) {
		t.Fatalf("expected no LLM request, got %d", len(chat.primaryRequests()))
	}
	if bb.waitForSentCount(1) {
		t.Fatalf("expected no reply send, got %+v", bb.sentMessages())
	}
}

func TestIMessageWebhookIgnoresSelfAuthoredMessage(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	rec := postIMessageWebhook(t, g, `{"type":"new-message","data":{"guid":"msg-1","text":"from oswald","isFromMe":true,"handle":{"address":"+15551234567"},"chats":[{"guid":"chat-direct","style":45}]}}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected no content, got %d", rec.Code)
	}
	if waitForPrimaryIMessageRequestCount(chat, 1, 100*time.Millisecond) {
		t.Fatalf("expected no LLM request, got %d", len(chat.primaryRequests()))
	}
}

func TestIMessageWebhookIgnoresUpdatedMessage(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	rec := postIMessageWebhook(t, g, `{"type":"updated-message","data":{"guid":"msg-1","text":"edited","isFromMe":false,"handle":{"address":"+15551234567"},"chats":[{"guid":"chat-direct","style":45}]}}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected no content, got %d", rec.Code)
	}
	if waitForPrimaryIMessageRequestCount(chat, 1, 100*time.Millisecond) {
		t.Fatalf("expected no LLM request, got %d", len(chat.primaryRequests()))
	}
}

func TestIMessageIgnoresUnmentionedGroupMessage(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "hello group",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	if len(primaryIMessageRequests(chat.requests)) != 0 {
		t.Fatalf("expected no LLM request, got %d", len(primaryIMessageRequests(chat.requests)))
	}
	if len(bb.sentMessages()) != 0 {
		t.Fatalf("expected no sent messages, got %+v", bb.sentMessages())
	}
}

func TestIMessageReplyToBotIncludesReplyContext(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()
	g.rememberBotMessage("bot-msg", "imessage:chat;+;group:+15551234567", "chat;+;group", "+15551234567", "prior bot answer")

	g.processIncomingMessage(webhookMessage{
		GUID:        "msg-2",
		Text:        "follow up",
		ReplyToGUID: "bot-msg",
		Handle:      messageHandle{Address: "+15551234567"},
		Chats:       []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	primary := primaryIMessageRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	prompt := primary[0].Messages[len(primary[0].Messages)-1].Content
	if !strings.Contains(prompt, "[Replying to Oswald: \"prior bot answer\"]") || !strings.Contains(prompt, "follow up") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

func TestIMessageAcceptedMessageStartsTypingAndMarksRead(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "@Oswald hello",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	if !bb.waitForPath("/api/v1/chat/chat%3B+%3Bgroup/typing") {
		t.Fatalf("expected escaped typing request, got paths %+v", bb.paths())
	}
	if !bb.waitForPath("/api/v1/chat/chat%3B+%3Bgroup/read") {
		t.Fatalf("expected escaped read receipt request, got paths %+v", bb.paths())
	}
	if sent := bb.sentMessages(); len(sent) != 1 || sent[0].Method != defaultSendMethod || sent[0].SelectedMessageGUID != "msg-1" {
		t.Fatalf("expected private reply send, got %+v", sent)
	}
}

func TestIMessageWebhookIgnoresTapback(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	body := `{"type":"new-message","data":{"guid":"tapback-1","text":"Liked an image","associatedMessageType":2001,"handle":{"address":"+15551234567"},"chats":[{"guid":"chat-direct","style":45}]}}`
	req := httptest.NewRequest(http.MethodPost, "/bluebubbles/webhook?password=pw", strings.NewReader(body))
	rec := httptest.NewRecorder()

	g.handleWebhook(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected no content, got %d", rec.Code)
	}
	if len(primaryIMessageRequests(chat.requests)) != 0 {
		t.Fatalf("expected no LLM request, got %d", len(primaryIMessageRequests(chat.requests)))
	}
}

func TestIMessageWebhookIgnoresAllTapbackCodes(t *testing.T) {
	codes := []int{2000, 2001, 2002, 2003, 2004, 2005, 3000, 3001, 3002, 3003, 3004, 3005}
	for _, code := range codes {
		for _, asString := range []bool{false, true} {
			name := fmt.Sprintf("%d", code)
			if asString {
				name += " string"
			}
			t.Run(name, func(t *testing.T) {
				bb := newFakeBlueBubbles(t)
				g, b, chat := newIMessageTestGateway(t, bb.server.URL)
				defer b.Shutdown()
				defer bb.server.Close()

				associatedType := fmt.Sprintf("%d", code)
				if asString {
					associatedType = fmt.Sprintf("%q", associatedType)
				}
				body := fmt.Sprintf(`{"type":"new-message","data":{"guid":"tapback-1","text":"Liked an image","associatedMessageType":%s,"handle":{"address":"+15551234567"},"chats":[{"guid":"chat-direct","style":45}]}}`, associatedType)
				rec := postIMessageWebhook(t, g, body)

				if rec.Code != http.StatusNoContent {
					t.Fatalf("expected no content, got %d", rec.Code)
				}
				if waitForPrimaryIMessageRequestCount(chat, 1, 50*time.Millisecond) {
					t.Fatalf("expected no LLM request, got %d", len(chat.primaryRequests()))
				}
			})
		}
	}
}

func TestIMessageWebhookMissingRequiredFieldsSkips(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
	}{
		{
			name: "missing chat guid",
			body: `{"type":"new-message","data":{"guid":"msg-1","text":"hello","isFromMe":false,"handle":{"address":"+15551234567"}}}`,
			code: http.StatusAccepted,
		},
		{
			name: "missing sender",
			body: `{"type":"new-message","data":{"guid":"msg-1","text":"hello","isFromMe":false,"chats":[{"guid":"chat-direct","style":45}]}}`,
			code: http.StatusAccepted,
		},
		{
			name: "empty text and no attachments",
			body: `{"type":"new-message","data":{"guid":"msg-1","text":"","isFromMe":false,"handle":{"address":"+15551234567"},"chats":[{"guid":"chat-direct","style":45}]}}`,
			code: http.StatusNoContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bb := newFakeBlueBubbles(t)
			g, b, chat := newIMessageTestGateway(t, bb.server.URL)
			defer b.Shutdown()
			defer bb.server.Close()

			rec := postIMessageWebhook(t, g, tt.body)

			if rec.Code != tt.code {
				t.Fatalf("expected status %d, got %d", tt.code, rec.Code)
			}
			if waitForPrimaryIMessageRequestCount(chat, 1, 100*time.Millisecond) {
				t.Fatalf("expected no LLM request, got %d", len(chat.primaryRequests()))
			}
		})
	}
}

func TestIMessageReadReceiptNotSentForIgnoredGroupMessage(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "hello group",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	if bb.waitForPath("/api/v1/chat/chat%3B+%3Bgroup/read") {
		t.Fatalf("expected no read receipt for ignored message, got paths %+v", bb.paths())
	}
}

func TestIMessageTypingAndReadRequirePrivateAPIHelper(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	bb.setCapabilities(false, true)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "hello imessage",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat-direct", Style: chatStyleDirect}},
	})

	if bb.waitForPath("/api/v1/chat/chat-direct/typing") {
		t.Fatalf("expected no typing request without private api, got paths %+v", bb.paths())
	}
	if bb.waitForPath("/api/v1/chat/chat-direct/read") {
		t.Fatalf("expected no read receipt without private api, got paths %+v", bb.paths())
	}
}

func TestIMessageCapabilityRetryStopsWhenHelperReady(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	bb.setCapabilities(true, false)
	bb.setHelperReadyAfter(3)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	if !g.refreshBlueBubblesCapabilitiesWithRetry(5, 0) {
		t.Fatal("expected capabilities to become available")
	}
	if attempts := bb.serverInfoRequests(); attempts != 3 {
		t.Fatalf("expected 3 server info attempts, got %d", attempts)
	}
}

func TestIMessageCapabilityRetryStopsAfterLimit(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	bb.setCapabilities(true, false)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	if g.refreshBlueBubblesCapabilitiesWithRetry(5, 0) {
		t.Fatal("expected capabilities to remain unavailable")
	}
	if attempts := bb.serverInfoRequests(); attempts != 5 {
		t.Fatalf("expected 5 server info attempts, got %d", attempts)
	}
}

func TestIMessageEndpointBuilders(t *testing.T) {
	endpoint, err := buildBlueBubblesMessageEndpoint("http://bb/base/", "a/b", "pw")
	if err != nil {
		t.Fatalf("message endpoint: %v", err)
	}
	if !strings.Contains(endpoint, "/base/api/v1/message/a%2Fb") || strings.Contains(endpoint, "a%252Fb") || !strings.Contains(endpoint, "password=pw") || !strings.Contains(endpoint, "with=") {
		t.Fatalf("unexpected message endpoint %q", endpoint)
	}
	attachment, err := buildBlueBubblesAttachmentEndpoint("http://bb", "att/1", "pw")
	if err != nil {
		t.Fatalf("attachment endpoint: %v", err)
	}
	if !strings.Contains(attachment, "/api/v1/attachment/att%2F1/download") || strings.Contains(attachment, "att%252F1") || !strings.Contains(attachment, "password=pw") || !strings.Contains(attachment, "original=true") {
		t.Fatalf("unexpected attachment endpoint %q", attachment)
	}
	chatAction, err := buildBlueBubblesChatActionEndpoint("http://bb/base/", "chat;+;group", "typing", "pw")
	if err != nil {
		t.Fatalf("chat action endpoint: %v", err)
	}
	if !strings.Contains(chatAction, "/base/api/v1/chat/chat%3B+%3Bgroup/typing") || !strings.Contains(chatAction, "password=pw") {
		t.Fatalf("unexpected chat action endpoint %q", chatAction)
	}
}

func TestIMessageEndpointBuildersEncodeSpecialPasswordAndGUIDs(t *testing.T) {
	message, err := buildBlueBubblesMessageEndpoint("http://bb/base/", "a/b c", "TestPass234!")
	if err != nil {
		t.Fatalf("message endpoint: %v", err)
	}
	parsedMessage, err := url.Parse(message)
	if err != nil {
		t.Fatalf("parse message endpoint: %v", err)
	}
	if got := parsedMessage.Query().Get("password"); got != "TestPass234!" {
		t.Fatalf("unexpected message password %q", got)
	}
	if !strings.Contains(parsedMessage.EscapedPath(), "/base/api/v1/message/a%2Fb%20c") || strings.Contains(parsedMessage.EscapedPath(), "%252F") {
		t.Fatalf("unexpected message path %q", parsedMessage.EscapedPath())
	}

	attachment, err := buildBlueBubblesAttachmentEndpoint("http://bb", "att/1 c", "abc&def")
	if err != nil {
		t.Fatalf("attachment endpoint: %v", err)
	}
	parsedAttachment, err := url.Parse(attachment)
	if err != nil {
		t.Fatalf("parse attachment endpoint: %v", err)
	}
	if got := parsedAttachment.Query().Get("password"); got != "abc&def" {
		t.Fatalf("unexpected attachment password %q", got)
	}
	if !strings.Contains(parsedAttachment.EscapedPath(), "/api/v1/attachment/att%2F1%20c/download") || strings.Contains(parsedAttachment.EscapedPath(), "%252F") {
		t.Fatalf("unexpected attachment path %q", parsedAttachment.EscapedPath())
	}

	chatAction, err := buildBlueBubblesChatActionEndpoint("http://bb", "chat;+;group", "typing", "abc&def")
	if err != nil {
		t.Fatalf("chat action endpoint: %v", err)
	}
	parsedChatAction, err := url.Parse(chatAction)
	if err != nil {
		t.Fatalf("parse chat action endpoint: %v", err)
	}
	if got := parsedChatAction.Query().Get("password"); got != "abc&def" {
		t.Fatalf("unexpected chat action password %q", got)
	}
	if !strings.Contains(parsedChatAction.EscapedPath(), "/api/v1/chat/chat%3B+%3Bgroup/typing") || strings.Contains(parsedChatAction.EscapedPath(), "%253B") {
		t.Fatalf("unexpected chat action path %q", parsedChatAction.EscapedPath())
	}
}

func TestChooseContactDisplayNameFallbackOrder(t *testing.T) {
	tests := []struct {
		name     string
		contacts []contactRecord
		expect   string
	}{
		{
			name:     "display name",
			contacts: []contactRecord{{DisplayName: "Alice Display", FirstName: "Alice", LastName: "Person", Nickname: "Al"}},
			expect:   "Alice Display",
		},
		{
			name:     "first last",
			contacts: []contactRecord{{FirstName: "Alice", LastName: "Person", Nickname: "Al"}},
			expect:   "Alice Person",
		},
		{
			name:     "nickname",
			contacts: []contactRecord{{Nickname: "Al"}},
			expect:   "Al",
		},
		{
			name:     "empty",
			contacts: []contactRecord{{}},
			expect:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseContactDisplayName(tt.contacts); got != tt.expect {
				t.Fatalf("expected %q, got %q", tt.expect, got)
			}
		})
	}
}

type imFakeChatter struct {
	mu        sync.Mutex
	requests  []llm.ChatRequest
	principal identity.Principal
}

func (f *imFakeChatter) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.principal, _ = requestctx.PrincipalFromContext(ctx)
	f.mu.Unlock()
	if req.Format == "json_object" {
		return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: `{"session_updates":{"summary":"","open_threads":[],"decisions":[],"user_goals":[]},"memory_candidates":[]}`}}, nil
	}
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "imessage response"}}, nil
}

func (f *imFakeChatter) lastPrincipal() identity.Principal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.principal
}

func primaryIMessageRequests(requests []llm.ChatRequest) []llm.ChatRequest {
	out := make([]llm.ChatRequest, 0, len(requests))
	for _, req := range requests {
		if req.Format != "json_object" {
			out = append(out, req)
		}
	}
	return out
}

func (f *imFakeChatter) primaryRequests() []llm.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return primaryIMessageRequests(append([]llm.ChatRequest(nil), f.requests...))
}

func postIMessageWebhook(t *testing.T, g *Gateway, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/bluebubbles/webhook?password=pw", strings.NewReader(body))
	rec := httptest.NewRecorder()
	g.handleWebhook(rec, req)
	return rec
}

func waitForPrimaryIMessageRequests(t *testing.T, chat *imFakeChatter, count int) []llm.ChatRequest {
	t.Helper()
	if !waitForPrimaryIMessageRequestCount(chat, count, time.Second) {
		t.Fatalf("expected %d primary LLM requests, got %d", count, len(chat.primaryRequests()))
	}
	return chat.primaryRequests()
}

func waitForPrimaryIMessageRequestCount(chat *imFakeChatter, count int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(chat.primaryRequests()) >= count {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return len(chat.primaryRequests()) >= count
}

type fakeBlueBubbles struct {
	server           *httptest.Server
	mu               sync.Mutex
	sent             []sendTextRequest
	seenPaths        []string
	privateAPI       bool
	helperConnected  bool
	serverInfoCount  int
	helperReadyAfter int
}

func newFakeBlueBubbles(t *testing.T) *fakeBlueBubbles {
	t.Helper()
	bb := &fakeBlueBubbles{}
	bb.privateAPI = true
	bb.helperConnected = true
	bb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		bb.mu.Lock()
		bb.seenPaths = append(bb.seenPaths, r.URL.EscapedPath())
		privateAPI := bb.privateAPI
		helperConnected := bb.helperConnected
		if r.URL.Path == "/api/v1/server/info" {
			bb.serverInfoCount++
			if bb.helperReadyAfter > 0 && bb.serverInfoCount >= bb.helperReadyAfter {
				bb.helperConnected = true
				helperConnected = true
			}
		}
		bb.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/server/info":
			_ = json.NewEncoder(w).Encode(serverInfoResponse{Data: struct {
				PrivateAPI      bool `json:"private_api"`
				HelperConnected bool `json:"helper_connected"`
			}{PrivateAPI: privateAPI, HelperConnected: helperConnected}})
		case r.URL.Path == "/api/v1/contact/query":
			_, _ = w.Write([]byte(`{"data":[{"displayName":"Alice"}]}`))
		case strings.Contains(r.URL.Path, "/typing"):
			_, _ = w.Write([]byte(`{"data":{"guid":"typing"}}`))
		case r.URL.Path == "/api/v1/message/text":
			var req sendTextRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode send request: %v", err)
			}
			bb.mu.Lock()
			bb.sent = append(bb.sent, req)
			bb.mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"guid":"sent-1"}}`))
		case r.URL.Path == "/api/v1/message/query":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"guid":"ok"}}`))
		}
	}))
	return bb
}

func (bb *fakeBlueBubbles) setCapabilities(privateAPI, helperConnected bool) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.privateAPI = privateAPI
	bb.helperConnected = helperConnected
}

func (bb *fakeBlueBubbles) setHelperReadyAfter(attempt int) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.helperReadyAfter = attempt
}

func (bb *fakeBlueBubbles) serverInfoRequests() int {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return bb.serverInfoCount
}

func (bb *fakeBlueBubbles) paths() []string {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return append([]string(nil), bb.seenPaths...)
}

func (bb *fakeBlueBubbles) waitForPath(path string) bool {
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, seen := range bb.paths() {
			if seen == path {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func (bb *fakeBlueBubbles) sentMessages() []sendTextRequest {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return append([]sendTextRequest(nil), bb.sent...)
}

func (bb *fakeBlueBubbles) sentMessage() string {
	sent := bb.sentMessages()
	if len(sent) == 0 {
		return ""
	}
	return sent[len(sent)-1].Message
}

func (bb *fakeBlueBubbles) waitForSentCount(count int) bool {
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(bb.sentMessages()) >= count {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return len(bb.sentMessages()) >= count
}

func newIMessageTestGateway(t *testing.T, blueBubblesURL string) (*Gateway, *broker.Broker, *imFakeChatter) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	memories := testutil.NewMemoryStore(t, dbPath, log)
	links := accountlinking.NewService(dbPath, memories, nil, log)
	soulPath := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(soulPath, []byte("You are Oswald."), 0o600); err != nil {
		t.Fatalf("write soul fixture: %v", err)
	}
	soulStore := soul.NewStore(soulPath)
	chat := &imFakeChatter{}
	ai := agent.NewAgent(chat, registry.New(log), "test-model", soulStore, memories, promptbudget.ContextBudget{PromptLimit: 100000}, governance.GlobalPolicy{MaxExecutions: 12, MaxToolIterations: 8}, log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	commandService, err := commands.NewServiceWithCommands()
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	g := &Gateway{
		BlueBubblesURL:      blueBubblesURL,
		BlueBubblesPassword: "pw",
		Links:               links,
		Runtime:             gatewayruntime.Dependencies{Commands: commandService, Log: log},
		Log:                 log,
		Broker:              b,
		messageIndex:        make(map[string]messageContext),
		contactNames:        make(map[string]contactNameCacheEntry),
	}
	return g, b, chat
}
