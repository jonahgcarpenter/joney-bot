package globalmemory

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

type handler struct {
	store *global.Store
	log   *config.Logger
}

// New creates the administrator-only global-memory command family.
func New(store *global.Store, log *config.Logger) commands.Handler {
	return &handler{store: store, log: log}
}

func (h *handler) Definition() commands.Definition {
	return commands.Definition{
		Name:      "global-memory",
		Summary:   "Manage administrator-curated global memory.",
		Usage:     "/global-memory <add <memory text>|list [page]|forget <id>>",
		AdminOnly: true,
	}
}

func (h *handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if h.store == nil {
		return commands.Result{}, fmt.Errorf("global memory is unavailable")
	}
	if len(req.Args) == 0 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	switch strings.ToLower(req.Args[0]) {
	case "add":
		return h.add(ctx, req)
	case "list":
		return h.list(ctx, req)
	case "forget":
		return h.forget(ctx, req)
	default:
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
}

func (h *handler) add(ctx context.Context, req commands.Request) (commands.Result, error) {
	text := strings.TrimSpace(strings.TrimPrefix(req.ArgsText, req.Args[0]))
	if text == "" {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	normalized := global.NormalizeMemory(text)
	if normalized == "" || utf8.RuneCountInString(normalized) > global.MaxMemoryRunes {
		return commands.Result{Text: fmt.Sprintf("Could not add global memory: global memory must contain 1..%d characters", global.MaxMemoryRunes), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	result, err := h.store.Add(ctx, text)
	if err != nil {
		return commands.Result{}, err
	}
	if result.Duplicate {
		return commands.Result{Text: fmt.Sprintf("Global memory already exists as ID %d.", result.Memory.ID), Outcome: commands.Outcome{Status: "ok", ReasonCode: "no_op"}}, nil
	}
	if h.log != nil {
		h.log.With(requestctx.LogFields(ctx)...).Info("global_memory.add.complete", "added global memory", config.F("request_id", req.RequestID), config.F("actor_user_id", req.Principal.CanonicalUserID), config.F("global_memory_id", result.Memory.ID), config.F("status", "ok"))
	}
	return commands.Result{Text: fmt.Sprintf("Added global memory %d.", result.Memory.ID)}, nil
}

func (h *handler) list(ctx context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) > 2 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	page := 1
	if len(req.Args) == 2 {
		var err error
		page, err = positiveDecimal(req.Args[1])
		if err != nil {
			return commands.Result{Text: "Global memory page must be a positive decimal integer.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
		}
	}
	result, err := h.store.List(ctx, page)
	if err != nil {
		return commands.Result{}, err
	}
	lines := []string{fmt.Sprintf("Global memory - page %d", page)}
	if len(result.Memories) == 0 {
		lines = append(lines, "No global memories found.")
	}
	for _, memory := range result.Memories {
		lines = append(lines, fmt.Sprintf("%d | %s", memory.ID, memory.Text))
	}
	if result.HasMore {
		lines = append(lines, "", fmt.Sprintf("More results: /global-memory list %d", page+1))
	}
	return commands.Result{Text: strings.Join(lines, "\n")}, nil
}

func (h *handler) forget(ctx context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) != 2 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	id, err := positiveDecimal64(req.Args[1])
	if err != nil {
		return commands.Result{Text: "Global memory ID must be a positive decimal integer.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	forgotten, err := h.store.Forget(ctx, id)
	if err != nil {
		return commands.Result{}, err
	}
	if !forgotten {
		return commands.Result{Text: fmt.Sprintf("Global memory %d was not found.", id), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "not_found"}}, nil
	}
	if h.log != nil {
		h.log.With(requestctx.LogFields(ctx)...).Info("global_memory.forget.complete", "forgot global memory", config.F("request_id", req.RequestID), config.F("actor_user_id", req.Principal.CanonicalUserID), config.F("global_memory_id", id), config.F("status", "ok"))
	}
	return commands.Result{Text: fmt.Sprintf("Forgot global memory %d.", id)}, nil
}

func positiveDecimal(value string) (int, error) {
	parsed, err := positiveDecimal64(value)
	if err != nil || parsed > int64(^uint(0)>>1) {
		return 0, fmt.Errorf("invalid positive decimal")
	}
	return int(parsed), nil
}

func positiveDecimal64(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("empty decimal")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid decimal")
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid positive decimal")
	}
	return parsed, nil
}
