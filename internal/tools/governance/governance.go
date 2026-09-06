// Package governance provides request-local limits and outcome tracking for
// model-visible tool execution.
package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// Outcome classifies a successful tool execution by whether it produced useful
// information for the active request.
type Outcome string

const (
	OutcomeProductive   Outcome = "productive"
	OutcomeUnproductive Outcome = "unproductive"
)

// Result is the shared result returned by builtin and MCP tools.
type Result struct {
	Content     string
	Outcome     Outcome
	ReasonCode  string
	IsDegraded  bool
	Attachments []media.OutputAttachment
}

// HistoryMode controls how much of a completed tool call may be retained in
// durable session history.
type HistoryMode string

const (
	HistoryNone     HistoryMode = "none"
	HistoryMetadata HistoryMode = "metadata"
	HistoryFull     HistoryMode = "full"
)

const (
	defaultHistoryArgumentBytes = 16 * 1024
	defaultHistoryResultRunes   = 16_000
)

// HistoryPolicy controls durable and searchable tool-call history. An empty
// mode defaults to full history so existing registered tools retain continuity.
type HistoryPolicy struct {
	Mode             HistoryMode
	SearchResult     bool
	MaxArgumentBytes int
	MaxResultRunes   int
}

// Effective returns the policy with compatibility defaults applied.
func (p HistoryPolicy) Effective() HistoryPolicy {
	if p.Mode == "" {
		p.Mode = HistoryFull
		p.SearchResult = true
	}
	if p.MaxArgumentBytes == 0 {
		p.MaxArgumentBytes = defaultHistoryArgumentBytes
	}
	if p.MaxResultRunes == 0 {
		p.MaxResultRunes = defaultHistoryResultRunes
	}
	return p
}

// ArgumentNormalizer returns the semantic argument value used for duplicate
// detection. It must not retain or mutate the supplied map.
type ArgumentNormalizer func(map[string]interface{}) interface{}

// ToolPolicy controls request-local limits for one model-visible tool.
type ToolPolicy struct {
	MaxExecutions   int
	MaxFailures     int
	MaxUnproductive int
	BlockDuplicates bool
	NormalizeArgs   ArgumentNormalizer
	History         HistoryPolicy
}

// Validate rejects negative limits. A zero limit disables that guard.
func (p ToolPolicy) Validate() error {
	if p.MaxExecutions < 0 {
		return fmt.Errorf("max executions must not be negative")
	}
	if p.MaxFailures < 0 {
		return fmt.Errorf("max failures must not be negative")
	}
	if p.MaxUnproductive < 0 {
		return fmt.Errorf("max unproductive results must not be negative")
	}
	history := p.History.Effective()
	if history.Mode != HistoryNone && history.Mode != HistoryMetadata && history.Mode != HistoryFull {
		return fmt.Errorf("invalid history mode %q", history.Mode)
	}
	if history.MaxArgumentBytes < 0 || history.MaxResultRunes < 0 {
		return fmt.Errorf("history limits must not be negative")
	}
	return nil
}

// GlobalPolicy controls request-wide tool-loop limits.
type GlobalPolicy struct {
	MaxExecutions     int
	MaxToolIterations int
}

// DefaultGlobalPolicy returns the fixed request-wide safety ceilings.
func DefaultGlobalPolicy() GlobalPolicy {
	return GlobalPolicy{MaxExecutions: 50, MaxToolIterations: 30}
}

// Validate checks the request-wide limits.
func (p GlobalPolicy) Validate() error {
	if p.MaxExecutions <= 0 {
		return fmt.Errorf("max executions must be positive")
	}
	if p.MaxToolIterations <= 0 {
		return fmt.Errorf("max tool iterations must be positive")
	}
	return nil
}

// ToolStats contains request-local counters for one tool.
type ToolStats struct {
	Attempts     int
	Executions   int
	Productive   int
	Unproductive int
	Failures     int
	Duplicates   int
	Blocked      int
}

// Decision is the governor's pre-execution decision for one model-emitted call.
type Decision struct {
	Allowed     bool
	ReasonCode  string
	fingerprint string
}

const (
	ReasonDuplicate          = "duplicate_call"
	ReasonToolLimit          = "tool_execution_limit"
	ReasonToolFailures       = "tool_failure_limit"
	ReasonToolUnproductive   = "tool_unproductive_limit"
	ReasonGlobalLimit        = "global_execution_limit"
	ReasonIterationLimit     = "global_iteration_limit"
	ReasonUnadvertised       = "tool_not_advertised"
	ReasonInvalidFingerprint = "invalid_arguments"
)

// Governor enforces one request's tool policies. It is intentionally not safe
// for concurrent use because Agent.Process executes one tool batch serially.
type Governor struct {
	global          GlobalPolicy
	stats           map[string]*ToolStats
	seen            map[string]struct{}
	toolIterations  int
	totalExecutions int
	globalReason    string
}

// New creates request-local governance state.
func New(global GlobalPolicy) *Governor {
	return &Governor{
		global: global,
		stats:  make(map[string]*ToolStats),
		seen:   make(map[string]struct{}),
	}
}

// BeginToolIteration records a model response containing at least one tool call.
func (g *Governor) BeginToolIteration() Decision {
	g.toolIterations++
	if g.toolIterations > g.global.MaxToolIterations {
		g.setGlobalReason(ReasonIterationLimit)
		return Decision{ReasonCode: ReasonIterationLimit}
	}
	return Decision{Allowed: true}
}

// BeforeExecution determines whether one advertised model call may execute.
func (g *Governor) BeforeExecution(name string, args map[string]interface{}, policy ToolPolicy, advertised bool) Decision {
	stats := g.toolStats(name)
	stats.Attempts++
	if !advertised {
		stats.Blocked++
		return Decision{ReasonCode: ReasonUnadvertised}
	}
	if g.globalReason != "" {
		stats.Blocked++
		return Decision{ReasonCode: g.globalReason}
	}
	if reason := retiredReason(*stats, policy); reason != "" {
		stats.Blocked++
		return Decision{ReasonCode: reason}
	}
	if g.totalExecutions >= g.global.MaxExecutions {
		g.setGlobalReason(ReasonGlobalLimit)
		stats.Blocked++
		return Decision{ReasonCode: ReasonGlobalLimit}
	}
	decision := Decision{Allowed: true}
	if policy.BlockDuplicates {
		fingerprint, err := Fingerprint(name, args, policy.NormalizeArgs)
		if err != nil {
			stats.Blocked++
			return Decision{ReasonCode: ReasonInvalidFingerprint}
		}
		if _, exists := g.seen[fingerprint]; exists {
			stats.Duplicates++
			stats.Blocked++
			return Decision{ReasonCode: ReasonDuplicate}
		}
		g.seen[fingerprint] = struct{}{}
		decision.fingerprint = fingerprint
	}
	stats.Executions++
	g.totalExecutions++
	return decision
}

// RecordResult accounts for a completed permitted execution.
func (g *Governor) RecordResult(name string, decision Decision, result Result, execErr error) {
	stats := g.toolStats(name)
	if execErr != nil {
		if decision.fingerprint != "" {
			delete(g.seen, decision.fingerprint)
		}
		stats.Failures++
		return
	}
	if result.Outcome != OutcomeProductive {
		stats.Unproductive++
		return
	}
	stats.Productive++
}

// IsToolRetired reports whether one tool has exhausted a per-tool limit.
func (g *Governor) IsToolRetired(name string, policy ToolPolicy) bool {
	return retiredReason(*g.toolStats(name), policy) != ""
}

// GlobalStopReason returns the reason all tools must be removed, if any.
func (g *Governor) GlobalStopReason() string {
	if g.globalReason == "" {
		switch {
		case g.totalExecutions >= g.global.MaxExecutions:
			g.setGlobalReason(ReasonGlobalLimit)
		case g.toolIterations >= g.global.MaxToolIterations:
			g.setGlobalReason(ReasonIterationLimit)
		}
	}
	return g.globalReason
}

// Stats returns a copy of one tool's counters.
func (g *Governor) Stats(name string) ToolStats { return *g.toolStats(name) }

// TotalExecutions returns the number of handlers invoked in this request.
func (g *Governor) TotalExecutions() int { return g.totalExecutions }

// ToolIterations returns the number of model responses containing tool calls.
func (g *Governor) ToolIterations() int { return g.toolIterations }

func (g *Governor) toolStats(name string) *ToolStats {
	stats := g.stats[name]
	if stats == nil {
		stats = &ToolStats{}
		g.stats[name] = stats
	}
	return stats
}

func (g *Governor) setGlobalReason(reason string) {
	if g.globalReason == "" {
		g.globalReason = reason
	}
}

func retiredReason(stats ToolStats, policy ToolPolicy) string {
	if policy.MaxExecutions > 0 && stats.Executions >= policy.MaxExecutions {
		return ReasonToolLimit
	}
	if policy.MaxFailures > 0 && stats.Failures >= policy.MaxFailures {
		return ReasonToolFailures
	}
	if policy.MaxUnproductive > 0 && stats.Unproductive >= policy.MaxUnproductive {
		return ReasonToolUnproductive
	}
	return ""
}

// Fingerprint returns a request-local-safe digest for a tool name and canonical
// argument value. encoding/json deterministically sorts string map keys.
func Fingerprint(name string, args map[string]interface{}, normalize ArgumentNormalizer) (string, error) {
	var value interface{} = args
	if normalize != nil {
		value = normalize(args)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode normalized tool arguments: %w", err)
	}
	digest := sha256.Sum256(append(append([]byte(name), 0), encoded...))
	return hex.EncodeToString(digest[:]), nil
}
