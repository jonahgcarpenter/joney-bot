package memories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	memorypkg "github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

func TestMemoriesListAndForget(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	memory := memorytest.NewStore(t, path, log)
	defer memory.Close() // nolint:errcheck
	accounts := accounts.NewService(path, memory, nil, log)
	defer accounts.Close() // nolint:errcheck
	userID, err := accounts.EnsureAccount(context.Background(), "homeassistant", "actor", "Actor")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := accounts.EnsureAccount(context.Background(), "homeassistant", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	var first memorypkg.MemoryEntry
	for i := 0; i < 30; i++ {
		entry, err := memorytest.PublishMemory(ctx, memory, userID, memorytest.MemoryFixture{Scope: memorypkg.ScopeLongTerm, Category: "notes", Statement: fmt.Sprintf("memory %02d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = entry
		}
	}
	if _, err := memorytest.PublishMemory(ctx, memory, otherID, memorytest.MemoryFixture{Scope: memorypkg.ScopeLongTerm, Category: "identity", Statement: "other tenant secret"}); err != nil {
		t.Fatal(err)
	}
	h := handler{accounts: accounts, memory: memory}
	principal := identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: "actor", Assurance: identity.AssuranceHomeAssistantToken}
	request := commands.Request{RequestID: "list", Principal: principal, Args: []string{"list"}}
	for _, args := range [][]string{nil, {"list", "extra"}, {"forget"}, {"unknown"}} {
		invalid := request
		invalid.Args = args
		result, err := h.Execute(ctx, invalid)
		if err != nil || result.Text != commands.UsageText(h.Definition()) {
			t.Fatalf("args=%v result=%+v err=%v", args, result, err)
		}
	}
	invalidID := request
	invalidID.Args = []string{"forget", "01"}
	if result, err := h.Execute(ctx, invalidID); err != nil || !strings.Contains(result.Text, "exact positive decimal") {
		t.Fatalf("invalid ID result=%+v err=%v", result, err)
	}
	result, err := h.Execute(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	attachments := result.Attachments
	if len(attachments) != 1 || attachments[0].MIMEType != "text/plain; charset=utf-8" {
		t.Fatalf("attachments=%+v", attachments)
	}
	listed := string(attachments[0].Data)
	if strings.Count(listed, "ID: ") != 30 || !strings.Contains(listed, "Memory: memory 29") || strings.Contains(listed, "other tenant secret") {
		t.Fatalf("unexpected memory list:\n%s", listed)
	}

	request.RequestID = "forget-one"
	request.Args = []string{"forget", fmt.Sprint(first.ID)}
	result, err = h.Execute(ctx, request)
	if err != nil || result.Invalidation != nil || !strings.Contains(result.Text, "source transcript and summaries remain") || !strings.Contains(result.Text, "can be learned again") {
		t.Fatalf("forget result=%+v err=%v", result, err)
	}

	request.RequestID = "forget-all"
	request.Args = []string{"forget", "all"}
	result, err = h.Execute(ctx, request)
	if err != nil || result.Invalidation == nil || !strings.Contains(result.Text, "observations, suppression rules, user MCP configuration") {
		t.Fatalf("forget all result=%+v err=%v", result, err)
	}
	remaining, err := memory.ListActiveMemories(ctx, userID, time.Now().UTC())
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining=%+v err=%v", remaining, err)
	}
	other, err := memory.ListActiveMemories(ctx, otherID, time.Now().UTC())
	if err != nil || len(other) != 1 {
		t.Fatalf("other tenant memories=%+v err=%v", other, err)
	}
}

func TestMemoriesUsageValidation(t *testing.T) {
	definition := (handler{}).Definition()
	if definition.Name != "memories" || definition.Usage != usage || !definition.UserExclusive {
		t.Fatalf("definition=%+v", definition)
	}
	if result, err := (handler{}).Execute(context.Background(), commands.Request{}); err == nil || result.Text != "" {
		t.Fatalf("nil service result=%+v err=%v", result, err)
	}
}

func TestListResultAlwaysUsesAttachment(t *testing.T) {
	result, err := listResult(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Attachments) != 1 || string(result.Attachments[0].Data) != "No active memories.\n" || result.Text == "" {
		t.Fatalf("result=%+v", result)
	}
}

func TestListResultRejectsInvalidUTF8(t *testing.T) {
	_, err := listResult([]memorypkg.ListedMemory{{ID: 1, Category: "notes", Statement: string([]byte{0xff})}})
	if err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("err=%v", err)
	}
}

func TestListProvenanceUsesPlainLabels(t *testing.T) {
	result, err := listResult([]memorypkg.ListedMemory{
		{ID: 1, Statement: "direct", Provenance: "user_statement", Confidence: 0.91},
		{ID: 2, Statement: "hypothesis", Provenance: "model_inference", Confidence: 0.42},
		{ID: 3, Statement: "legacy", Provenance: "legacy_import", Confidence: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(result.Attachments[0].Data)
	for _, label := range []string{"Source: stated", "Source: inferred", "Source: unknown"} {
		if !strings.Contains(text, label) {
			t.Errorf("missing %q in %q", label, text)
		}
	}
	if strings.Contains(text, "%") || strings.Contains(text, "Confidence") || strings.Contains(text, "0.42") {
		t.Fatalf("raw confidence in default list: %q", text)
	}
}

func TestMemoryExportPreservesMultipartUTF8(t *testing.T) {
	content := strings.Repeat("\u754c", commands.MaxAttachmentBytes/3+3)
	result, err := exportResult(content, "oswald-memory-observations", "attached")
	if err != nil || len(result.Attachments) != 2 {
		t.Fatalf("multipart count=%d err=%v", len(result.Attachments), err)
	}
	var joined strings.Builder
	for i, attachment := range result.Attachments {
		if !utf8.Valid(attachment.Data) || len(attachment.Data) > commands.MaxAttachmentBytes || attachment.Filename != fmt.Sprintf("oswald-memory-observations.part%03d.txt", i+1) {
			t.Fatalf("invalid part %d", i)
		}
		joined.Write(attachment.Data)
	}
	if joined.String() != content {
		t.Fatal("multipart export lost content")
	}
	if _, err := exportResult(strings.Repeat("x", maxMemoryListBytes+1), "oswald-memories", "attached"); err == nil {
		t.Fatal("oversized export must fail, not truncate")
	}
}

func TestMemorySuppressionCommandsFenceOwnerAndReportOutcomes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	store := memorytest.NewStore(t, path, log)
	defer store.Close()
	accountService := accounts.NewService(path, store, nil, log)
	defer accountService.Close()
	userID, err := accountService.EnsureAccount(ctx, "homeassistant", "actor", "Actor")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := accountService.EnsureAccount(ctx, "homeassistant", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := memorytest.PublishMemory(ctx, store, userID, memorytest.MemoryFixture{Scope: memorypkg.ScopeLongTerm, Category: "notes", Statement: "Synthetic Atlas deployment"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := memorytest.PublishMemory(ctx, store, otherID, memorytest.MemoryFixture{Scope: memorypkg.ScopeLongTerm, Category: "notes", Statement: "Foreign memory canary"})
	if err != nil {
		t.Fatal(err)
	}
	h := handler{accounts: accountService, memory: store}
	req := commands.Request{Principal: identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: "actor", Assurance: identity.AssuranceHomeAssistantToken}}
	for _, args := range [][]string{{"suppress", "all"}, {"unsuppress", "01"}, {"correct", "1"}, {"suppress", fmt.Sprint(other.ID)}} {
		req.Args = args
		result, err := h.Execute(ctx, req)
		if err != nil || result.Outcome.Status != "rejected" || result.Outcome.IsChanged {
			t.Fatalf("args=%v outcome=%+v err=%v", args, result.Outcome, err)
		}
	}
	for _, args := range [][]string{{"list"}, {"observations"}, {"suppressions"}, {"suppress", fmt.Sprint(entry.ID)}, {"unsuppress", "1"}, {"forget", "all"}} {
		stale := req
		stale.Args = args
		stale.Principal.CanonicalUserID = otherID
		if _, err := h.Execute(ctx, stale); err == nil {
			t.Fatalf("stale canonical principal accepted for %v", args)
		}
	}
	req.Args = []string{"suppress", fmt.Sprint(entry.ID)}
	result, err := h.Execute(ctx, req)
	if err != nil || result.Outcome.Operation != "memory.suppress" || !result.Outcome.IsChanged || result.Outcome.AffectedCount != 1 || !strings.Contains(result.Text, "exact identified canonical claim") || !strings.Contains(result.Text, "was deleted") {
		t.Fatalf("suppress outcome=%+v err=%v", result, err)
	}
	if _, err := store.EntryByID(entry.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("suppression did not physically delete memory: %v", err)
	}
	rules, err := store.ListMemorySuppressions(ctx, userID)
	if err != nil || len(rules) != 1 {
		t.Fatalf("rules=%+v err=%v", rules, err)
	}
	for _, command := range []string{"observations", "suppressions"} {
		req.Args = []string{command}
		result, err := h.Execute(ctx, req)
		if err != nil || len(result.Attachments) != 1 || strings.Contains(string(result.Attachments[0].Data), "Foreign memory canary") {
			t.Fatalf("%s result=%+v err=%v", command, result, err)
		}
	}
	req.Args = []string{"unsuppress", fmt.Sprint(rules[0].ID)}
	result, err = h.Execute(ctx, req)
	if err != nil || result.Outcome.Operation != "memory.unsuppress" || !result.Outcome.IsChanged || !strings.Contains(result.Text, "not restored") {
		t.Fatalf("unsuppress result=%+v err=%v", result, err)
	}
	result, err = h.Execute(ctx, req)
	if err != nil || result.Outcome.ReasonCode != "not_found" || result.Outcome.IsChanged {
		t.Fatalf("repeated unsuppress result=%+v err=%v", result, err)
	}
}

func TestMemoryObservationsExportExpiryAndForgetAll(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	store := memorytest.NewStore(t, path, log)
	defer store.Close()
	accountService := accounts.NewService(path, store, nil, log)
	defer accountService.Close()
	userID, err := accountService.EnsureAccount(ctx, "homeassistant", "actor", "Actor")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := accountService.EnsureAccount(ctx, "homeassistant", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	// Deliberate expiry and tenant-isolation fixtures; no publication worker runs.
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	var firstTurnID int64
	for i, fixture := range []struct {
		owner, text string
		expires     time.Time
	}{
		{userID, "temporary observation", now.Add(time.Hour)},
		{userID, "expired observation canary", now.Add(-time.Hour)},
		{otherID, "foreign observation canary", now.Add(time.Hour)},
	} {
		session := fmt.Sprintf("observation-fixture-%d", i)
		profile, err := store.ResolveSessionProfile(ctx, fixture.owner, session, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		turn, err := memorytest.AppendPendingTurn(ctx, store, session, fixture.owner, profile.Generation, "synthetic evidence", "Synthetic acknowledgement.", nil, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.MarkSessionTurnDelivered(ctx, fixture.owner, turn.ID); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstTurnID = turn.ID
		}
		// The API uses the wall clock; backdate only this deliberate expiry fixture.
		if fixture.expires.Before(now) {
			if _, err := db.Exec(`UPDATE session_turns SET created_at = ? WHERE id = ?`, now.Add(-2*time.Hour).Format(time.RFC3339Nano), turn.ID); err != nil {
				t.Fatal(err)
			}
		}
		_, err = db.Exec(`INSERT INTO memory_observations(canonical_user_id,source_turn_id,statement,evidence,context,provenance_type,claim_slot,claim_value,observed_at,expires_at) SELECT canonical_user_id,id,?,?,?,?,?,?,created_at,? FROM session_turns WHERE id = ?`, fixture.text, "synthetic evidence", "synthetic context", "user_statement", "notes.fact", fixture.text, fixture.expires.Format(time.RFC3339Nano), turn.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	entry, err := memorytest.PublishMemory(ctx, store, userID, memorytest.MemoryFixture{Statement: "Synthetic suppressed fact"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SuppressMemory(ctx, userID, entry.ID, now); err != nil {
		t.Fatal(err)
	}
	h := handler{accounts: accountService, memory: store}
	req := commands.Request{Principal: identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: "actor", Assurance: identity.AssuranceHomeAssistantToken}, Args: []string{"observations"}}
	result, err := h.Execute(ctx, req)
	if err != nil || len(result.Attachments) != 1 {
		t.Fatalf("export err=%v", err)
	}
	text := string(result.Attachments[0].Data)
	for _, want := range []string{"Observation: temporary observation", "Evidence: synthetic evidence", "Context: synthetic context", "Source: stated", "Expires: " + now.Add(time.Hour).Format(time.RFC3339), fmt.Sprintf("Source turn: %d", firstTurnID)} {
		if !strings.Contains(text, want) {
			t.Errorf("missing field %q", want)
		}
	}
	if strings.Contains(text, "canary") {
		t.Fatal("expired or foreign observation leaked")
	}
	req.Args = []string{"forget", "all"}
	if _, err := h.Execute(ctx, req); err != nil {
		t.Fatal(err)
	}
	observations, err := store.ListMemoryObservations(ctx, userID, now)
	if err != nil || len(observations) != 0 {
		t.Fatalf("observations remain after forget-all: %v", err)
	}
	rules, err := store.ListMemorySuppressions(ctx, userID)
	if err != nil || len(rules) != 0 {
		t.Fatalf("rules remain after forget-all: %v", err)
	}
}
