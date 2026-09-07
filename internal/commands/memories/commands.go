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

const usage = "/memories list | observations | forget <id|all> | suppress <id> | suppressions | unsuppress <rule-id>"

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
	return commands.Definition{Name: "memories", Summary: "Inspect, forget, or suppress your memories.", Usage: usage, UserExclusive: true}
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
	if len(req.Args) == 1 && (req.Args[0] == "observations" || req.Args[0] == "suppressions") {
		userID, err := h.resolveUser(req)
		if err != nil {
			return commands.Result{}, err
		}
		var content strings.Builder
		if req.Args[0] == "observations" {
			observations, err := h.memory.ListMemoryObservations(ctx, userID, time.Now().UTC())
			if err != nil {
				return commands.Result{}, err
			}
			content.WriteString("Temporary observations, not durable facts. Only unexpired observations are shown.\n\n")
			for _, observation := range observations {
				fmt.Fprintf(&content, "ID: %d\nObservation: %s\nEvidence: %s\nContext: %s\nSource: %s\nObserved: %s\nExpires: %s\nSource turn: %d\nClaim: %s = %s\n\n", observation.ID, observation.Statement, observation.Evidence, observation.Context, memory.ProvenanceLabel(observation.Provenance), observation.ObservedAt.UTC().Format(time.RFC3339), observation.ExpiresAt.UTC().Format(time.RFC3339), observation.SourceTurnID, observation.ClaimSlot, observation.ClaimValue)
			}
			if len(observations) == 0 {
				content.WriteString("No unexpired observations.\n")
			}
		} else {
			rules, err := h.memory.ListMemorySuppressions(ctx, userID)
			if err != nil {
				return commands.Result{}, err
			}
			content.WriteString("Suppression rules block durable publication of the exact identified canonical claim, not all paraphrases or all content copies.\n\n")
			for _, rule := range rules {
				fmt.Fprintf(&content, "Rule ID: %d\nMemory: %s\nClaim: %s = %s\n\n", rule.ID, rule.Statement, rule.ClaimSlot, rule.ClaimValue)
			}
			if len(rules) == 0 {
				content.WriteString("No suppression rules.\n")
			}
		}
		return exportResult(content.String(), "oswald-memory-"+req.Args[0], "Your memory "+req.Args[0]+" are attached.")
	}
	if len(req.Args) != 2 || (req.Args[0] != "forget" && req.Args[0] != "suppress" && req.Args[0] != "unsuppress") {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if req.Args[0] == "forget" && strings.EqualFold(req.Args[1], "all") {
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
		return commands.Result{Text: "Your learned memories, observations, suppression rules, user MCP configuration, session transcripts, summaries, jobs, and account-link challenges were deleted. Your account, linked identities, moderation settings, and speaker intro were preserved; sessions were reset. Future conversations can teach new memories. External logs, backups, and delivered messages are not erased.", Invalidation: &invalidation.Event{SessionIDs: sessionIDs}, Outcome: commands.Outcome{Status: "ok", Operation: "memory.forget_all", IsChanged: true, AffectedCount: 1}}, nil
	}
	id, err := memory.ParseMemoryID(req.Args[1])
	if err != nil {
		return commands.Result{Text: "ID must be an exact positive decimal stable ID. Only forget accepts all.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	var ruleID int64
	err = h.accounts.RunAuthenticatedCanonicalMutation(req.Principal, func(userID string) error {
		if userID != req.Principal.CanonicalUserID {
			return accounts.ErrPrincipalMismatch
		}
		switch req.Args[0] {
		case "suppress":
			var err error
			ruleID, err = h.memory.SuppressMemory(ctx, userID, id, time.Now().UTC())
			return err
		case "unsuppress":
			changed, err := h.memory.UnsuppressMemory(ctx, userID, id)
			if err == nil && !changed {
				return sql.ErrNoRows
			}
			return err
		default:
			return h.memory.HardDeleteMemory(ctx, userID, id, time.Now().UTC())
		}
	})
	if errors.Is(err, sql.ErrNoRows) {
		return commands.Result{Text: "Memory or suppression rule " + req.Args[1] + " was not found.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "not_found"}}, nil
	} else if err != nil {
		return commands.Result{}, err
	}
	switch req.Args[0] {
	case "suppress":
		return commands.Result{Text: fmt.Sprintf("Memory %d was deleted and suppression rule %d now blocks durable publication of its exact identified canonical claim. This does not block every paraphrase or erase transcripts, delivered messages, logs, or backups.", id, ruleID), Outcome: commands.Outcome{Status: "ok", Operation: "memory.suppress", IsChanged: true, AffectedCount: 1}}, nil
	case "unsuppress":
		return commands.Result{Text: fmt.Sprintf("Suppression rule %d was disabled. The memory was not restored; new evidence can teach the claim again.", id), Outcome: commands.Outcome{Status: "ok", Operation: "memory.unsuppress", IsChanged: true, AffectedCount: 1}}, nil
	default:
		return commands.Result{Text: fmt.Sprintf("Memory %d was permanently deleted. Its source transcript and summaries remain, so the claim can be learned again. Use /memories suppress <id> on an active memory to block durable publication of that exact identified canonical claim.", id), Outcome: commands.Outcome{Status: "ok", Operation: "memory.forget", IsChanged: true, AffectedCount: 1}}, nil
	}
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
		for _, entry := range memories {
			fmt.Fprintf(&content, "ID: %d\nCategory: %s\nSource: %s\nMemory: %s\n\n", entry.ID, entry.Category, memory.ProvenanceLabel(entry.Provenance), entry.Statement)
		}
	}
	text := fmt.Sprintf("Your %d active memories are attached.", len(memories))
	if len(memories) == 1 {
		text = "Your 1 active memory is attached."
	} else if len(memories) == 0 {
		text = "Your memory list is attached."
	}
	return exportResult(content.String(), "oswald-memories", text)
}

func exportResult(content, basename, text string) (commands.Result, error) {
	data := []byte(content)
	if !utf8.Valid(data) {
		return commands.Result{}, fmt.Errorf("memory list contains invalid UTF-8")
	}
	if len(data) > maxMemoryListBytes {
		return commands.Result{}, fmt.Errorf("memory list exceeds the %d-byte UTF-8 attachment limit", maxMemoryListBytes)
	}
	parts := splitUTF8(data, commands.MaxAttachmentBytes)
	attachments := make([]commands.Attachment, 0, len(parts))
	for i, part := range parts {
		filename := basename + ".txt"
		if len(parts) > 1 {
			filename = fmt.Sprintf("%s.part%03d.txt", basename, i+1)
		}
		attachments = append(attachments, commands.Attachment{Filename: filename, MIMEType: "text/plain; charset=utf-8", Data: part})
	}
	result := commands.Result{Text: text, Attachments: attachments}
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
