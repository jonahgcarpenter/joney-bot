package budget

const (
	defaultContextWindow   = 32768
	defaultResponseReserve = 8192
	defaultSafetyMargin    = 256
)

// CompactionTriggerPercent is the input-pressure threshold for foreground and durable compaction.
const CompactionTriggerPercent = 70

// RecentTailLimit reserves 25 percent of input for recent exchanges, bounded to
// 2,000 through 8,000 tokens without exceeding the available input.
func RecentTailLimit(inputLimit int) int {
	limit := inputLimit / 4
	if limit < 2000 {
		limit = 2000
	}
	if limit > 8000 {
		limit = 8000
	}
	if limit > inputLimit {
		limit = inputLimit
	}
	return limit
}

// ContextBudget describes the request-time prompt budget derived from the
// active model's context window.
type ContextBudget struct {
	ContextWindow   int
	ResponseReserve int
	SafetyMargin    int
	PromptLimit     int
}

// UsableInputLimit returns the model input capacity after output and safety
// reserves. An explicit prompt limit can further constrain that capacity.
func (b ContextBudget) UsableInputLimit() int {
	limit := b.PromptLimit
	if b.ContextWindow > 0 {
		contextLimit := b.ContextWindow - b.ResponseReserve
		if contextLimit < 0 {
			contextLimit = 0
		}
		if limit <= 0 || contextLimit < limit {
			limit = contextLimit
		}
	}
	limit -= b.SafetyMargin
	if limit < 0 {
		return 0
	}
	return limit
}

// NewContextBudget derives prompt-budget settings from configured model limits.
// Non-positive limits use conservative package defaults.
func NewContextBudget(contextWindow, maxOutputTokens int) ContextBudget {
	budget := ContextBudget{
		ContextWindow:   defaultContextWindow,
		ResponseReserve: defaultResponseReserve,
		SafetyMargin:    defaultSafetyMargin,
	}
	if contextWindow > 0 {
		budget.ContextWindow = contextWindow
	}
	if maxOutputTokens > 0 {
		budget.ResponseReserve = maxOutputTokens
	}
	return budget
}
