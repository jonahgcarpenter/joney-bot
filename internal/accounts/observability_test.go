package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestPolicyReasonSeparatesBusinessAndStorageFailures(t *testing.T) {
	for _, err := range []error{ErrChallengeInvalid, fmt.Errorf("wrapped: %w", ErrGatewayConflict), policyError("not_found", "canonical user %q not found", "synthetic")} {
		if reason, ok := PolicyReason(err); !ok || reason == "" {
			t.Fatalf("expected policy classification: %v", err)
		}
	}
	if _, ok := PolicyReason(errors.New(ErrChallengeInvalid.Error())); ok {
		t.Fatal("classified an untyped error by text")
	}
	if _, ok := PolicyReason(errors.New("database failure")); ok {
		t.Fatal("classified operational failure as policy")
	}
}

func TestCanonicalCreationAuditPrecedesSpeakerIntroFailure(t *testing.T) {
	links := newTestService(t)
	file, err := os.CreateTemp(t.TempDir(), "audit")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stderr
	os.Stderr = file
	links.log = config.NewLogger(config.LevelDebug).Server("accounts")
	os.Stderr = old
	// The account transaction succeeds; only its subsequent speaker-intro sync fails.
	if err := links.memories.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req-audit", OperationID: "op-audit"})
	if _, err := links.EnsureAccount(ctx, "discord", "123456789", "synthetic-private-name"); err == nil {
		t.Fatal("expected speaker intro failure")
	}
	owner, found, err := links.ResolveAccount("discord", "123456789")
	if err != nil || !found {
		t.Fatalf("creation did not commit: found=%t err=%v", found, err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "123456789") || strings.Contains(string(data), "synthetic-private-name") {
		t.Fatal("external identity leaked")
	}
	foundAudit := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "account_link.canonical_user.created" {
			foundAudit = true
			if event["target_user_id"] != owner || event["request_id"] != "req-audit" || event["operation_id"] != "op-audit" {
				t.Fatalf("uncorrelated audit: %+v", event)
			}
		}
	}
	if !foundAudit {
		t.Fatal("committed creation was not audited")
	}
}
