package memory

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestSessionImagesDeliveryScopeBoundsAndReset(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	profile, err := s.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	appendImage := func(id string) StoredSessionTurn {
		t.Helper()
		turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "user", SessionID: "session", Generation: profile.Generation, UserText: "generate", AssistantText: "image", TTL: time.Hour, History: EmptyToolHistory(), Pressure: SessionPromptPressure{Limit: 1000, Version: "test"}, Images: []requestctx.InputImage{{ID: id, MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("synthetic normalized bytes"))}}})
		if err != nil {
			t.Fatal(err)
		}
		return turn
	}
	assertCount := func(user, session string, generation, want int) []requestctx.InputImage {
		t.Helper()
		images, err := s.SessionImages(ctx, user, session, generation)
		if err != nil || len(images) != want {
			t.Fatalf("images=%d want=%d err=%v", len(images), want, err)
		}
		return images
	}
	turn := appendImage("first")
	assertCount("user", "session", profile.Generation, 0)
	if err := s.MarkSessionTurnDeliveryFailed(ctx, "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	assertCount("user", "session", profile.Generation, 0)
	if err := s.MarkSessionTurnDelivered(ctx, "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	images := assertCount("user", "session", profile.Generation, 1)
	if images[0].ID != "first" || images[0].Data != base64.StdEncoding.EncodeToString([]byte("synthetic normalized bytes")) {
		t.Fatal("asset bytes changed")
	}
	assertCount("other", "session", profile.Generation, 0)
	assertCount("user", "other", profile.Generation, 0)
	assertCount("user", "session", profile.Generation+1, 0)
	for i := 0; i < 10; i++ {
		turn = appendImage(fmt.Sprintf("image-%d", i))
		if err := s.MarkSessionTurnDelivered(ctx, "user", turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	images = assertCount("user", "session", profile.Generation, 8)
	if images[0].ID != "image-9" || images[7].ID != "image-2" {
		t.Fatal("wrong eviction order")
	}
	if _, err := s.sql.Exec(`UPDATE session_turns SET expires_at=? WHERE id=?`, formatTime(time.Now().Add(-time.Minute)), turn.ID); err != nil {
		t.Fatal(err)
	}
	assertCount("user", "session", profile.Generation, 7)
	counts, err := s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.SessionImagesDeleted != 1 {
		t.Fatalf("deleted=%d err=%v", counts.SessionImagesDeleted, err)
	}
	if _, err := s.ResetSession(ctx, "user", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	assertCount("user", "session", profile.Generation, 0)
	var count int
	if err := s.sql.QueryRow(`SELECT count(*) FROM session_images`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained reset assets=%d err=%v", count, err)
	}
}

func TestSessionImageWriteRollsBackTurnOnInvalidAsset(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	profile, err := s.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"invalid base64", base64.StdEncoding.EncodeToString(make([]byte, 286721))} {
		_, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "user", SessionID: "session", Generation: profile.Generation, UserText: "generate", AssistantText: "image", History: EmptyToolHistory(), Pressure: SessionPromptPressure{Limit: 1000, Version: "test"}, Images: []requestctx.InputImage{{ID: "invalid", MIMEType: "image/png", Data: data}}})
		if err == nil {
			t.Fatal("invalid asset accepted")
		}
	}
	var count int
	if err := s.sql.QueryRow(`SELECT count(*) FROM session_turns`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial turns=%d err=%v", count, err)
	}
}

func TestSessionImagesFollowMergeGenerationAndForgetAll(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "winner", "loser")
	ctx := context.Background()
	for _, user := range []string{"winner", "loser"} {
		profile, err := s.ResolveSessionProfile(ctx, user, "session", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 6; i++ {
			turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: user, SessionID: "session", Generation: profile.Generation, UserText: "generate", AssistantText: "image", TTL: time.Hour, History: EmptyToolHistory(), Pressure: SessionPromptPressure{Limit: 1000, Version: "test"}, Images: []requestctx.InputImage{{ID: fmt.Sprintf("%s-%d", user, i), MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte(user))}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.MarkSessionTurnDelivered(ctx, user, turn.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := MergeUsersTx(ctx, tx, "winner", "loser", ""); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM session_images`).Scan(&count); err != nil || count != 8 {
		t.Fatalf("merged count=%d err=%v", count, err)
	}
	for _, scope := range []struct {
		user             string
		generation, want int
	}{{"loser", 1, 0}, {"winner", 1, 0}, {"winner", 2, 6}} {
		images, err := s.SessionImages(ctx, scope.user, "session", scope.generation)
		if err != nil || len(images) != scope.want {
			t.Fatalf("scope=%+v images=%d err=%v", scope, len(images), err)
		}
		if len(images) > 0 && images[0].Data != base64.StdEncoding.EncodeToString([]byte("loser")) {
			t.Fatal("merge lost image bytes")
		}
	}
	if _, err := s.ResetUserDataPreservingAccount(ctx, "winner", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM session_images`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("forgotten assets=%d err=%v", count, err)
	}
}

func TestSessionImagesReopenExpiryAndAccountDeletion(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprint(expire), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "images.db")
			s := newTestStore(path, config.NewLogger(config.LevelError))
			seedAccountUsers(t, s, "user")
			ctx := context.Background()
			profile, err := s.ResolveSessionProfile(ctx, "user", "session", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "user", SessionID: "session", Generation: profile.Generation, UserText: "generate", AssistantText: "image", TTL: time.Hour, History: EmptyToolHistory(), Pressure: SessionPromptPressure{Limit: 1000, Version: "test"}, Images: []requestctx.InputImage{{ID: "persistent-id", MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("retained bytes"))}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.MarkSessionTurnDelivered(ctx, "user", turn.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = newTestStore(path, config.NewLogger(config.LevelError))
			defer s.Close()
			images, err := s.SessionImages(ctx, "user", "session", profile.Generation)
			if err != nil || len(images) != 1 || images[0].ID != "persistent-id" || images[0].Data != base64.StdEncoding.EncodeToString([]byte("retained bytes")) {
				t.Fatal("reopen lost image reference or bytes")
			}
			if expire {
				_, err = s.sql.Exec(`UPDATE sessions SET expires_at=? WHERE canonical_user_id='user'`, formatTime(time.Now().Add(-time.Hour)))
			} else {
				_, err = s.sql.Exec(`DELETE FROM account_users WHERE canonical_user_id='user'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			images, err = s.SessionImages(ctx, "user", "session", profile.Generation)
			if err != nil || len(images) != 0 {
				t.Fatalf("expired/deleted images=%d err=%v", len(images), err)
			}
			if !expire {
				var count int
				if err := s.sql.QueryRow(`SELECT COUNT(*) FROM session_images`).Scan(&count); err != nil || count != 0 {
					t.Fatal("account deletion retained image bytes")
				}
			}
		})
	}
}
