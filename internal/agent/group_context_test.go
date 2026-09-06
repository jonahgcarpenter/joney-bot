package agent

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestProcessPersistsPublicTextSeparatelyFromInternalPrompt(t *testing.T) {
	for _, publicText := range []string{"  public question  ", ""} {
		t.Run(publicText, func(t *testing.T) {
			chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "public final answer", Thinking: "private reasoning"}}}}
			a, store := newTestAgent(t, chat, nil, nil)
			defer store.Close()
			meta := requestctx.Metadata{GroupGateway: "discord", GroupChatID: "chat:Exact;ID", PublicUserText: publicText}
			const internalPrompt = "[Replying to a message: \"quoted enrichment\"]\n\ninternal attachment enrichment"
			response, err := a.Process(requestctx.WithMetadata(context.Background(), meta), Request{
				RequestID: "group-request", SessionKey: "group-session", Prompt: internalPrompt,
				Principal: identity.Principal{CanonicalUserID: "user-1", ExternalID: "external-user", Gateway: "discord", Assurance: identity.AssuranceDiscordGateway},
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.SourceTurnID <= 0 {
				t.Fatal("missing persisted turn")
			}
			var gateway, chatID, public, internal, answer string
			var pending bool
			err = store.sql.QueryRow(`SELECT group_gateway, group_chat_id, public_user_text, user_text, assistant_text, delivered_at IS NULL FROM session_turns WHERE id = ?`, response.SourceTurnID).Scan(&gateway, &chatID, &public, &internal, &answer, &pending)
			if err != nil {
				t.Fatal(err)
			}
			if gateway != meta.GroupGateway || chatID != meta.GroupChatID || public != publicText || answer != "public final answer" || !pending {
				t.Fatalf("incorrect pending public exchange: %q %q %q %q pending=%t", gateway, chatID, public, answer, pending)
			}
			if internal == public || internal == "" || !messagesContain(chat.requests[0].Messages, internalPrompt) {
				t.Fatalf("public and internal prompt were not kept separate: public=%q internal=%q", public, internal)
			}
		})
	}
}
