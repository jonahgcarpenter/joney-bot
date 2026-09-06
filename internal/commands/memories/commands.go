package memories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/invalidation"
)

const usage = "/memories list | forget <id|all>"

const maxMemoryListBytes = commands.MaxTotalAttachmentBytes - (utf8.UTFMax-1)*(commands.MaxAttachments-1)

type handler struct {
	accounts *accounts.Service
	memory   *memory.Store
}

// New creates the memories command handler.
func New(accounts *accounts.Service, memory *memory.Store) commands.Handler {
	return handler{accounts: accounts, memory: memory}
}

func (handler) Definition() commands.Definition {
	return commands.Definition{Name: "memories", Summary: "List or forget your memories.", Usage: usage, UserExclusive: true}
}

func (h handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if h.accounts == nil || h.memory == nil {
		return commands.Result{}, fmt.Errorf("memory service is unavailable")
	}
	if len(req.Args) == 1 && req.Args[0] == "list" {
		userID, err := h.resolveUser(req)
		if err != nil {
			return commands.Result{}, err
		}
		memories, err := h.memory.ListActiveMemories(ctx, userID, time.Now().UTC())
		if err != nil {
			return commands.Result{}, err
		}
		return listResult(memories)
	}
	if len(req.Args) != 2 || req.Args[0] != "forget" {
		return commands.Result{Text: commands.UsageText(h.Definition())}, nil
	}
	if strings.EqualFold(req.Args[1], "all") {
		var sessionIDs []string
		if err := h.accounts.RunAuthenticatedCanonicalMutation(req.Principal, func(userID string) error {
			if userID != req.Principal.CanonicalUserID {
				return accounts.ErrPrincipalMismatch
			}
			var err error
			sessionIDs, err = h.memory.ResetUserDataPreservingAccount(ctx, userID, time.Now().UTC())
			return err
		}); err != nil {
			return commands.Result{}, err
		}
		h.accounts.UserDataResetCommitted(req.Principal.CanonicalUserID)
		return commands.Result{Text: "All stored information was permanently deleted. Your account was preserved and your sessions were reset.", Invalidation: &invalidation.Event{SessionIDs: sessionIDs}}, nil
	}
	id, err := memory.ParseMemoryID(req.Args[1])
	if err != nil {
		return commands.Result{Text: "ID must be an exact positive decimal stable ID, or all."}, nil
	}
	err = h.accounts.RunAuthenticatedCanonicalMutation(req.Principal, func(userID string) error {
		if userID != req.Principal.CanonicalUserID {
			return accounts.ErrPrincipalMismatch
		}
		return h.memory.HardDeleteMemory(ctx, userID, id, time.Now().UTC())
	})
	if errors.Is(err, sql.ErrNoRows) {
		return commands.Result{Text: "Memory " + req.Args[1] + " was not found."}, nil
	} else if err != nil {
		return commands.Result{}, err
	}
	return commands.Result{Text: "Memory " + req.Args[1] + " was permanently deleted."}, nil
}

func (h handler) resolveUser(req commands.Request) (string, error) {
	userID, err := h.accounts.ResolvePrincipal(req.Principal)
	if err != nil {
		return "", err
	}
	if userID != req.Principal.CanonicalUserID {
		return "", accounts.ErrPrincipalMismatch
	}
	return userID, nil
}

func listResult(memories []memory.ListedMemory) (commands.Result, error) {
	var content strings.Builder
	if len(memories) == 0 {
		content.WriteString("No active memories.\n")
	} else {
		for _, memory := range memories {
			fmt.Fprintf(&content, "ID: %d\nCategory: %s\nMemory: %s\n\n", memory.ID, memory.Category, memory.Statement)
		}
	}
	data := []byte(content.String())
	if !utf8.Valid(data) {
		return commands.Result{}, fmt.Errorf("memory list contains invalid UTF-8")
	}
	if len(data) > maxMemoryListBytes {
		return commands.Result{}, fmt.Errorf("memory list exceeds the %d-byte UTF-8 attachment limit", maxMemoryListBytes)
	}
	parts := splitUTF8(data, commands.MaxAttachmentBytes)
	attachments := make([]commands.Attachment, 0, len(parts))
	for i, part := range parts {
		filename := "oswald-memories.txt"
		if len(parts) > 1 {
			filename = fmt.Sprintf("oswald-memories.part%03d.txt", i+1)
		}
		attachments = append(attachments, commands.Attachment{Filename: filename, MIMEType: "text/plain; charset=utf-8", Data: part})
	}
	result := commands.Result{Text: fmt.Sprintf("Your %d active memories are attached.", len(memories)), Attachments: attachments}
	if len(memories) == 1 {
		result.Text = "Your 1 active memory is attached."
	} else if len(memories) == 0 {
		result.Text = "Your memory list is attached."
	}
	if err := result.ValidateAttachments(); err != nil {
		return commands.Result{}, fmt.Errorf("memory list cannot be delivered: %w", err)
	}
	return result, nil
}

func splitUTF8(data []byte, limit int) [][]byte {
	parts := make([][]byte, 0, (len(data)+limit-1)/limit)
	for len(data) > limit {
		end := limit
		for end > 0 && !utf8.RuneStart(data[end]) {
			end--
		}
		if end == 0 {
			end = limit
		}
		parts = append(parts, data[:end])
		data = data[end:]
	}
	return append(parts, data)
}
