package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func appendGroupTranscriptTestTurn(t *testing.T, store *Store, user, session, gateway, chat, public string, delivered bool) StoredSessionTurn {
	t.Helper()
	generation := bindTranscriptTestSession(t, store, user, session)
	turn, err := store.AppendPendingSessionTurn(context.Background(), SessionTurnWrite{
		UserID: user, SessionID: session, Generation: generation,
		UserText: "hiddenenrichment", PublicUserText: public, AssistantText: "publicanswer",
		GroupGateway: gateway, GroupChatID: chat, TTL: time.Hour,
		Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "test"},
		History: ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{{AssistantContent: "hiddenthinking", Calls: []ToolHistoryCall{{
			Name: "weather.current", Arguments: map[string]interface{}{"city": "hiddenargument"}, Status: "succeeded", Outcome: "productive", Result: "hiddentool", ExecutedAt: "2026-08-28T12:00:00Z", SearchResult: true,
		}}}}},
	})
	if err != nil || turn.ID == 0 {
		t.Fatalf("append turn=%+v err=%v", turn, err)
	}
	if delivered {
		if err := store.MarkSessionTurnDelivered(context.Background(), user, turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	return turn
}

func TestGroupTranscriptPublicOnlyAndExactIsolation(t *testing.T) {
	store := newTranscriptTestStore(t)
	seedAccountUsers(t, store, "caller", "source")
	generation := bindTranscriptTestSession(t, store, "caller", "caller-session")
	// Source generations are not required to equal the caller's generation.
	bindTranscriptTestSession(t, store, "source", "private-source-identifier")
	if _, err := store.sql.Exec(`UPDATE sessions SET generation = 7 WHERE canonical_user_id = 'source'`); err != nil {
		t.Fatal(err)
	}
	public := "  publicmarker exact visible prompt  "
	want := appendGroupTranscriptTestTurn(t, store, "source", "private-source-identifier", "discord", "chat-1", public, true)
	appendGroupTranscriptTestTurn(t, store, "caller", "another-chat", "discord", "chat 1", public, true)
	appendGroupTranscriptTestTurn(t, store, "source", "another-gateway", "imessage", "chat-1", public, true)
	appendGroupTranscriptTestTurn(t, store, "source", "legacy", "", "", public, true)
	appendGroupTranscriptTestTurn(t, store, "source", "pending", "discord", "chat-1", public, false)
	rebuildTestIndexes(t, store)
	for _, query := range []string{"publicmarker", "publicanswer", `publicmarker" OR tool_text:*`} {
		results, err := store.SearchGroupTranscript(context.Background(), "caller", "caller-session", generation, "discord", "chat-1", query, 10)
		if err != nil || len(results) != 1 {
			t.Fatalf("query=%q results=%+v err=%v", query, results, err)
		}
		got := results[0]
		if got.TurnID != want.ID || got.CanonicalUserID != "source" || got.SessionID != "" || got.SessionGeneration != 0 || len(got.Records) != 2 || got.Records[0].Role != "user" || got.Records[0].Content != public || got.Records[1].Role != "assistant" || got.Records[1].Content != "publicanswer" {
			t.Fatalf("unexpected public excerpt: %+v", got)
		}
		encoded, _ := json.Marshal(got)
		for _, secret := range []string{"hidden", "session_id", "session_generation", "private-source-identifier", "tool_calls", "tool_name"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("public output contains %q: %s", secret, encoded)
			}
		}
	}
	for _, hidden := range []string{"hiddenenrichment", "hiddenthinking", "hiddenargument", "hiddentool"} {
		results, err := store.SearchGroupTranscript(context.Background(), "caller", "caller-session", generation, "discord", "chat-1", hidden, 10)
		if err != nil || len(results) != 0 {
			t.Fatalf("hidden query=%q results=%+v err=%v", hidden, results, err)
		}
		private, err := store.SearchTranscript(context.Background(), "source", want.SessionID, want.Generation, hidden, 10)
		wantCount := 1
		if hidden == "hiddenthinking" || hidden == "hiddenargument" {
			wantCount = 0 // Private FTS indexes tool names/results, not intermediate content/arguments.
		}
		if err != nil || len(private) != wantCount || (wantCount == 1 && len(private[0].Records) != 4) {
			t.Fatalf("private behavior changed: query=%q results=%+v err=%v", hidden, private, err)
		}
	}
}

func TestGroupTranscriptCanonicalFencesRejectStaleIndex(t *testing.T) {
	for name, mutation := range map[string]string{
		"caller inactive":   `UPDATE sessions SET is_active = 0 WHERE canonical_user_id = 'caller'`,
		"caller expired":    `UPDATE sessions SET expires_at = '2000-01-01T00:00:00Z' WHERE canonical_user_id = 'caller'`,
		"caller generation": `UPDATE sessions SET generation = generation + 1 WHERE canonical_user_id = 'caller'`,
		"source inactive":   `UPDATE sessions SET is_active = 0 WHERE canonical_user_id = 'source'`,
		"source expired":    `UPDATE sessions SET expires_at = '2000-01-01T00:00:00Z' WHERE canonical_user_id = 'source'`,
		"source generation": `UPDATE sessions SET generation = generation + 1 WHERE canonical_user_id = 'source'`,
		"pending":           `UPDATE session_turns SET delivered_at = NULL`,
		"failed":            `UPDATE session_turns SET delivery_failed_at = created_at`,
		"deleted":           `DELETE FROM session_turns`,
		"index owner":       `UPDATE INDEX_TABLE SET canonical_user_id = 'caller'`,
		"index session":     `UPDATE INDEX_TABLE SET session_id = 'other'`,
		"index generation":  `UPDATE INDEX_TABLE SET session_generation = 99`,
		"index gateway":     `UPDATE INDEX_TABLE SET group_gateway = 'discord'`,
		"index chat":        `UPDATE INDEX_TABLE SET group_chat_id = 'other'`,
		"index public text": `UPDATE INDEX_TABLE SET public_user_text = 'marker corrupted'`,
		"index answer":      `UPDATE INDEX_TABLE SET assistant_text = 'marker corrupted'`,
	} {
		t.Run(name, func(t *testing.T) {
			store := newTranscriptTestStore(t)
			seedAccountUsers(t, store, "caller", "source")
			generation := bindTranscriptTestSession(t, store, "caller", "caller-session")
			appendGroupTranscriptTestTurn(t, store, "source", "source-session", "imessage", "chat", "marker", true)
			rebuildTestIndexes(t, store)
			live, err := store.LiveIndexRevision(context.Background(), IndexKindTranscriptFTS)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.sql.Exec(strings.ReplaceAll(mutation, "INDEX_TABLE", live.TableName)); err != nil {
				t.Fatal(err)
			}
			results, err := store.SearchGroupTranscript(context.Background(), "caller", "caller-session", generation, "imessage", "chat", "marker", 5)
			if err != nil || len(results) != 0 {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestGroupTranscriptMergePreservesPublicProvenance(t *testing.T) {
	store := newTranscriptTestStore(t)
	seedAccountUsers(t, store, "caller", "winner", "loser")
	generation := bindTranscriptTestSession(t, store, "caller", "caller-session")
	turn := appendGroupTranscriptTestTurn(t, store, "loser", "source-session", "discord", "chat", "marker", true)
	rebuildTestIndexes(t, store)
	ctx := context.Background()
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "intro"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	results, err := store.SearchGroupTranscript(ctx, "caller", "caller-session", generation, "discord", "chat", "marker", 5)
	if err != nil || len(results) != 0 {
		t.Fatalf("stale owner results=%+v err=%v", results, err)
	}
	rebuildTestIndexes(t, store)
	results, err = store.SearchGroupTranscript(ctx, "caller", "caller-session", generation, "discord", "chat", "marker", 5)
	if err != nil || len(results) != 1 || results[0].TurnID != turn.ID || results[0].CanonicalUserID != "winner" || results[0].Records[0].Content != "marker" {
		t.Fatalf("merged results=%+v err=%v", results, err)
	}
}

func TestGroupTranscriptRequiresNewIndexSchema(t *testing.T) {
	store := newTranscriptTestStore(t)
	ctx := context.Background()
	_, err := store.SearchGroupTranscript(ctx, "caller", "session", 1, "discord", "chat", "marker", 5)
	if !errors.Is(err, ErrTranscriptSearchUnavailable) {
		t.Fatalf("missing index: %v", err)
	}
	rebuildTestIndexes(t, store)
	if _, err := store.sql.Exec(`UPDATE derived_index_revisions SET schema_version = 2 WHERE index_kind = 'transcript_fts' AND state = 'live'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.SearchGroupTranscript(ctx, "caller", "session", 1, "discord", "chat", "marker", 5)
	if !errors.Is(err, ErrTranscriptSearchUnavailable) {
		t.Fatalf("old index: %v", err)
	}
	needs, err := store.IndexRevisionNeedsRebuild(ctx, IndexKindTranscriptFTS)
	if err != nil || !needs {
		t.Fatalf("rebuild=%v err=%v", needs, err)
	}
}

func TestGroupTranscriptDeliveryResetAndForgetLifecycle(t *testing.T) {
	for _, action := range []string{"reset", "forget"} {
		t.Run(action, func(t *testing.T) {
			store := newTranscriptTestStore(t)
			seedAccountUsers(t, store, "caller", "source")
			generation := bindTranscriptTestSession(t, store, "caller", "caller-session")
			turn := appendGroupTranscriptTestTurn(t, store, "source", "source-session", "discord", "chat", "marker", false)
			ctx := context.Background()
			if err := store.MarkSessionTurnDeliveryFailed(ctx, "source", turn.ID); err != nil {
				t.Fatal(err)
			}
			rebuildTestIndexes(t, store)
			results, err := store.SearchGroupTranscript(ctx, "caller", "caller-session", generation, "discord", "chat", "marker", 5)
			if err != nil || len(results) != 0 {
				t.Fatalf("failed delivery results=%+v err=%v", results, err)
			}
			if err := store.MarkSessionTurnDelivered(ctx, "source", turn.ID); err != nil {
				t.Fatal(err)
			}
			rebuildTestIndexes(t, store)
			results, err = store.SearchGroupTranscript(ctx, "caller", "caller-session", generation, "discord", "chat", "marker", 5)
			if err != nil || len(results) != 1 {
				t.Fatalf("late delivery results=%+v err=%v", results, err)
			}
			if action == "reset" {
				_, err = store.ResetSession(ctx, "source", "source-session", time.Hour)
			} else {
				_, err = store.ResetUserDataPreservingAccount(ctx, "source", time.Now().UTC())
			}
			if err != nil {
				t.Fatal(err)
			}
			results, err = store.SearchGroupTranscript(ctx, "caller", "caller-session", generation, "discord", "chat", "marker", 5)
			if err != nil || len(results) != 0 {
				t.Fatalf("removed results=%+v err=%v", results, err)
			}
		})
	}
}

func TestGroupTranscriptLimitsAndInvalidScope(t *testing.T) {
	store := newTranscriptTestStore(t)
	seedAccountUsers(t, store, "caller", "source")
	generation := bindTranscriptTestSession(t, store, "caller", "caller-session")
	for range 12 {
		appendGroupTranscriptTestTurn(t, store, "source", "source-session", "discord", "chat", "marker", true)
	}
	appendGroupTranscriptTestTurn(t, store, "source", "source-session", "discord", "chat", "marker "+strings.Repeat("\u754c", maxTranscriptSearchChars), true)
	rebuildTestIndexes(t, store)
	ctx := context.Background()
	for _, requested := range []int{0, 1, 1000} {
		results, err := store.SearchGroupTranscript(ctx, "caller", "caller-session", generation, "discord", "chat", "marker", requested)
		want := requested
		if want == 0 {
			want = defaultTranscriptSearchLimit
		}
		if want > maxTranscriptSearchLimit {
			want = maxTranscriptSearchLimit
		}
		if err != nil || len(results) != want {
			t.Fatalf("limit=%d results=%+v err=%v", requested, results, err)
		}
		encoded, err := json.Marshal(results)
		if err != nil || len(encoded) > maxTranscriptSearchChars {
			t.Fatalf("bytes=%d err=%v", len(encoded), err)
		}
	}
	for _, scope := range [][2]string{{"", ""}, {"discord", ""}, {"", "chat"}, {"homeassistant", "chat"}, {"discord", " chat"}} {
		if _, err := store.SearchGroupTranscript(ctx, "caller", "caller-session", generation, scope[0], scope[1], "marker", 5); err == nil {
			t.Fatalf("invalid search scope accepted: %q", scope)
		}
		if scope == [2]string{"", ""} {
			continue
		}
		_, err := store.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "caller", SessionID: "caller-session", Generation: generation, AssistantText: "answer", GroupGateway: scope[0], GroupChatID: scope[1], Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "test"}})
		if err == nil {
			t.Fatalf("invalid write scope accepted: %q", scope)
		}
	}
}

func TestTranscriptOldLiveProjectionRemainsPrivateDuringRebuild(t *testing.T) {
	store := newTranscriptTestStore(t)
	seedAccountUsers(t, store, "source")
	turn := appendGroupTranscriptTestTurn(t, store, "source", "session", "discord", "chat", "marker", true)
	ctx := context.Background()
	revision, err := store.CreateIndexRevision(ctx, IndexKindTranscriptFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an on-disk schema-2 live projection from the preceding release.
	if _, err := store.sql.Exec(`DROP TABLE `+revision.TableName+`;
CREATE VIRTUAL TABLE `+revision.TableName+` USING fts5(canonical_user_id, session_id, session_generation, user_text, assistant_text, tool_text);
UPDATE derived_index_revisions SET schema_version = 2, state = 'live' WHERE id = ?`, revision.ID); err != nil {
		t.Fatal(err)
	}
	revision.SchemaVersion = 2
	record, err := store.TranscriptIndexRecordByID(ctx, turn.ID, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTranscriptIndexRecord(ctx, revision, record); err != nil {
		t.Fatal(err)
	}
	private, err := store.SearchTranscript(ctx, "source", "session", turn.Generation, "hiddentool", 5)
	if err != nil || len(private) != 1 {
		t.Fatalf("old private search=%+v err=%v", private, err)
	}
	_, err = store.SearchGroupTranscript(ctx, "source", "session", turn.Generation, "discord", "chat", "marker", 5)
	if !errors.Is(err, ErrTranscriptSearchUnavailable) {
		t.Fatalf("old group search: %v", err)
	}
	rebuildTestIndexes(t, store)
	group, err := store.SearchGroupTranscript(ctx, "source", "session", turn.Generation, "discord", "chat", "marker", 5)
	if err != nil || len(group) != 1 {
		t.Fatalf("new group search=%+v err=%v", group, err)
	}

	shadow, err := store.CreateIndexRevision(ctx, IndexKindTranscriptFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTranscriptIndexRecord(ctx, shadow, record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE ` + shadow.TableName + ` SET public_user_text = 'hiddenenrichment'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, shadow.ID); err == nil {
		t.Fatal("corrupt public projection published")
	}
}
