package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

type groupContextProcessor struct {
	calls   chan requestctx.Metadata
	prompts chan string
}

func (p groupContextProcessor) Process(ctx context.Context, req agent.Request) (*agent.Response, error) {
	p.calls <- requestctx.MetadataFromContext(ctx)
	p.prompts <- req.Prompt
	return &agent.Response{Response: "public answer"}, nil
}

func TestExecutePropagatesTrustedGroupContextThroughBroker(t *testing.T) {
	for _, tc := range []struct {
		name, gateway                 string
		group, direct, mention, reply bool
		publicText                    string
	}{
		{"discord group", "discord", true, false, true, false, "  public question  "},
		{"imessage reply", "imessage", true, false, false, true, "public question"},
		{"empty public input", "discord", true, false, true, false, ""},
		{"ambient", "discord", true, false, false, false, "ambient text"},
		{"discord DM", "discord", false, true, false, false, "private text"},
		{"imessage DM", "imessage", false, true, false, false, "private text"},
		{"HA", "homeassistant", false, true, false, false, "private text"},
		{"unspecified scope", "discord", false, false, true, false, "private text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := groupContextProcessor{calls: make(chan requestctx.Metadata, 1), prompts: make(chan string, 1)}
			log := config.NewLogger(config.LevelError)
			b := broker.NewBroker(p, 1, log)
			b.Start()
			defer b.Shutdown()
			principal := testPrincipal("user")
			principal.Gateway = tc.gateway
			switch tc.gateway {
			case "discord":
				principal.Assurance = identity.AssuranceDiscordGateway
			case "imessage":
				principal.Assurance = identity.AssuranceBlueBubblesWebhook
			}
			req := Request{RequestID: "request", Principal: principal, ChatID: "chat:Exact;ID", SessionKey: "discord:fake:session", IsGroup: tc.group, IsDirect: tc.direct, IsMention: tc.mention, IsReplyToBot: tc.reply, Text: "cleaned internal text", PublicUserText: tc.publicText,
				Reply: &routing.ReplyContext{Text: "quoted enrichment"}, Unsupported: []string{"attachment enrichment"}}
			responder := &fakeResponder{}
			outcome := Execute(req, Dependencies{Broker: b, Log: log}, responder)
			if tc.group && !tc.mention && !tc.reply {
				if outcome.Action != routing.ActionIgnore || len(p.calls) != 0 || responder.started {
					t.Fatalf("ambient message invoked processor: %+v", outcome)
				}
				return
			}
			if outcome.Err != nil || len(p.calls) != 1 {
				t.Fatalf("missing processor invocation: %+v", outcome)
			}
			meta, prompt := <-p.calls, <-p.prompts
			if meta.RequestID != req.RequestID || meta.Workload != "foreground" {
				t.Fatalf("lost correlation: %+v", meta)
			}
			if tc.group {
				if meta.GroupGateway != tc.gateway || meta.GroupChatID != req.ChatID || meta.PublicUserText != tc.publicText {
					t.Fatalf("incorrect public provenance: %+v", meta)
				}
			} else if meta.GroupGateway != "" || meta.GroupChatID != "" || meta.PublicUserText != "" {
				t.Fatalf("private request shared: %+v", meta)
			}
			if !strings.Contains(prompt, req.Text) || !strings.Contains(prompt, "quoted enrichment") || !strings.Contains(prompt, "attachment enrichment") {
				t.Fatalf("missing internal enrichment: %q", prompt)
			}
		})
	}
}

func TestExecuteRejectsMalformedGroupContext(t *testing.T) {
	for _, tc := range []struct {
		name, chat                  string
		direct, ha, unauthenticated bool
	}{
		{name: "missing chat"},
		{name: "blank chat", chat: " \t"},
		{name: "padded chat", chat: " chat "},
		{name: "conflicting flags", chat: "chat", direct: true},
		{name: "unsupported gateway", chat: "chat", ha: true},
		{name: "unauthenticated", chat: "chat", unauthenticated: true},
	} {
		for _, text := range []string{"public question", "/ping"} {
			t.Run(tc.name+text, func(t *testing.T) {
				log, records := telemetryLogger(t)
				principal := identity.Principal{CanonicalUserID: "user", Gateway: "discord", ExternalID: "external", Assurance: identity.AssuranceDiscordGateway}
				if tc.ha {
					principal = testPrincipal("user")
				}
				if tc.unauthenticated {
					principal.Assurance = identity.AssuranceSelfAsserted
				}
				responder := &fakeResponder{}
				outcome := Execute(Request{Principal: principal, ChatID: tc.chat, IsGroup: true, IsDirect: tc.direct, IsMention: true, Text: text}, Dependencies{Log: log}, responder)
				want := "invalid_group_context"
				if tc.unauthenticated {
					want = "invalid_principal"
				}
				if outcome.Reason != want || responder.agentErr == "" || responder.started || responder.command.Text != "" {
					t.Fatalf("malformed request not rejected: %+v responder=%+v", outcome, responder)
				}
				complete := 0
				for _, record := range records() {
					if record["event"] == "gateway.request.received" {
						t.Fatal("invalid scope admitted")
					}
					if record["event"] == "gateway.request.complete" {
						complete++
						if record["status"] != "rejected" || record["reason_code"] != want || record["is_admitted"] != false {
							t.Fatalf("incorrect rejection telemetry: %+v", record)
						}
					}
				}
				if complete != 1 {
					t.Fatalf("completion count=%d", complete)
				}
			})
		}
	}
}
