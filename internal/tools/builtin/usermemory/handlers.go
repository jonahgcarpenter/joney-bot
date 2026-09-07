package usermemory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

type foregroundMemorySaveItem struct {
	Statement          string  `json:"statement"`
	Evidence           string  `json:"evidence"`
	Category           string  `json:"category"`
	ClaimSlot          string  `json:"claim_slot"`
	ClaimValue         string  `json:"claim_value"`
	Supersedes         string  `json:"supersedes"`
	EvidenceType       string  `json:"evidence_type"`
	Confidence         float64 `json:"confidence"`
	ReinforcesMemoryID int64   `json:"reinforces_memory_id,omitempty"`
	TargetMemoryID     int64   `json:"target_memory_id,omitempty"`
	Retention          string  `json:"retention,omitempty"`
	Intent             string  `json:"intent,omitempty"`
	Context            string  `json:"context,omitempty"`
	TTLDays            int     `json:"ttl_days,omitempty"`
	Cardinality        string  `json:"cardinality,omitempty"`
	ExpectedRevision   int64   `json:"expected_revision,omitempty"`
}

type foregroundMemorySaveResult struct {
	Status         string                           `json:"status"`
	StagedCount    int                              `json:"staged_count"`
	RejectedCount  int                              `json:"rejected_count"`
	Publication    string                           `json:"publication"`
	Message        string                           `json:"message"`
	Results        []foregroundMemorySaveItemResult `json:"results"`
	RequiredAction string                           `json:"required_action,omitempty"`
}

type foregroundMemorySaveItemResult struct {
	Index                    int      `json:"index"`
	Status                   string   `json:"status"`
	ReasonCode               string   `json:"reason_code"`
	Reason                   string   `json:"reason"`
	Retryable                bool     `json:"retryable"`
	AllowedClaimSlotPrefixes []string `json:"allowed_claim_slot_prefixes,omitempty"`
}

func requestLog(log *config.Logger, ctx context.Context) *config.Logger {
	meta := requestctx.MetadataFromContext(ctx)
	principal, _ := requestctx.PrincipalFromContext(ctx)
	return log.Agent("agent.tool.memory", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
}

func authenticatedPrincipal(ctx context.Context, toolName string) (identity.Principal, error) {
	principal, _ := requestctx.PrincipalFromContext(ctx)
	if !principal.Valid() || !principal.Authenticated() {
		return identity.Principal{}, fmt.Errorf("%s: authenticated user identity is required", toolName)
	}
	return principal, nil
}

// NewSaveHandler returns a Handler that stages foreground memory candidates.
// Durable publication is intentionally left to the successful-delivery path.
func NewSaveHandler(log *config.Logger) func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		principal, err := authenticatedPrincipal(ctx, toolnames.UserMemorySave)
		if err != nil {
			return governance.Result{}, err
		}
		collector := requestctx.MemoryStageCollectorFromContext(ctx)
		if collector == nil {
			return governance.Result{}, fmt.Errorf("%s: request memory staging is unavailable", toolnames.UserMemorySave)
		}
		sourceText := requestctx.MetadataFromContext(ctx).CurrentUserText
		if strings.TrimSpace(sourceText) == "" {
			return governance.Result{}, fmt.Errorf("%s: current user text is required", toolnames.UserMemorySave)
		}
		items, err := decodeForegroundMemorySave(args)
		if err != nil {
			return governance.Result{}, fmt.Errorf("%s: %w", toolnames.UserMemorySave, err)
		}

		staged := make([]requestctx.StagedMemoryCandidate, 0, len(items))
		rejected := 0
		results := make([]foregroundMemorySaveItemResult, 0, len(items))
		hasRetryable := false
		for index, item := range items {
			provenance := policy.ProvenanceUserStatement
			if item.EvidenceType == "model_inference" {
				provenance = policy.ProvenanceModelInference
			}
			targetID := item.TargetMemoryID
			if targetID == 0 {
				targetID = item.ReinforcesMemoryID
			}
			candidate := memory.ForegroundMemoryCandidate{
				Statement: item.Statement, Evidence: item.Evidence, Category: policy.Category(item.Category),
				ClaimSlot: item.ClaimSlot, ClaimValue: item.ClaimValue, EvidenceType: item.EvidenceType,
				Provenance: provenance, Confidence: item.Confidence, TargetMemoryID: targetID,
				SupersedesStatement: item.Supersedes, Retention: item.Retention, Intent: item.Intent,
				Context: item.Context, TTLDays: item.TTLDays, Cardinality: item.Cardinality, ExpectedRevision: item.ExpectedRevision,
			}
			if err := memory.ValidateForegroundMemoryCandidate(&candidate, false); err != nil {
				rejected++
				hasRetryable = true
				result := foregroundMemorySaveItemResult{Index: index, Status: "rejected", ReasonCode: "invalid_candidate", Reason: "check retention, intent, target, revision, TTL, cardinality and category-compatible claim identity", Retryable: true}
				if (candidate.Intent == "correction" || candidate.Intent == "retire") && (candidate.TargetMemoryID <= 0 || candidate.ExpectedRevision <= 0) {
					result.ReasonCode = "missing_target_revision"
					result.Reason = "Use user_memory_search or user_memory_list to obtain the target memory ID, revision and claim slot, then retry with target_memory_id and expected_revision. Never invent them; supersedes alone is insufficient."
				}
				if !policy.AssessmentClaimSlotCompatible(candidate.Category, candidate.ClaimSlot) {
					result.ReasonCode = "invalid_claim_slot"
					result.AllowedClaimSlotPrefixes = foregroundClaimSlotPrefixes(item.Category)
				}
				results = append(results, result)
				continue
			}
			output, err := candidate.Evaluate(sourceText)
			if err != nil {
				rejected++
				hasRetryable = true
				results = append(results, foregroundMemorySaveItemResult{Index: index, Status: "rejected", ReasonCode: "invalid_candidate", Reason: "candidate failed structural validation", Retryable: true})
				continue
			}
			if output.Approval == policy.ApprovalRejected {
				rejected++
				code, safeReason, retryable := foregroundMemoryRejection(output.Reason)
				hasRetryable = hasRetryable || retryable
				result := foregroundMemorySaveItemResult{Index: index, Status: "rejected", ReasonCode: code, Reason: safeReason, Retryable: retryable}
				if code == "invalid_claim_slot" {
					result.AllowedClaimSlotPrefixes = foregroundClaimSlotPrefixes(item.Category)
				}
				results = append(results, result)
				continue
			}
			staged = append(staged, requestctx.StagedMemoryCandidate{CanonicalUserID: principal.CanonicalUserID, Candidate: output, TargetMemoryID: targetID, SupersedesStatement: strings.TrimSpace(item.Supersedes), Retention: candidate.Retention, Intent: candidate.Intent, Context: candidate.Context, TTLDays: candidate.TTLDays, Cardinality: candidate.Cardinality, ExpectedRevision: candidate.ExpectedRevision})
			status := "staged_active"
			if output.Approval == policy.ApprovalProposed {
				status = "staged_candidate"
			}
			if candidate.Retention == "observation" {
				status = "staged_observation"
			}
			if candidate.Intent == "retire" {
				status = "staged_retirement"
			}
			results = append(results, foregroundMemorySaveItemResult{Index: index, Status: status, ReasonCode: "pending_delivery", Reason: output.Reason})
		}
		if len(staged) > 0 {
			if err := ctx.Err(); err != nil {
				return governance.Result{}, err
			}
			if err := collector.Stage(staged); err != nil {
				return governance.Result{}, fmt.Errorf("%s: %w", toolnames.UserMemorySave, err)
			}
		}
		response := foregroundMemorySaveResult{
			Status: map[bool]string{true: "staged", false: "rejected"}[len(staged) > 0], Publication: "pending_successful_delivery",
			StagedCount: len(staged), RejectedCount: rejected, Results: results,
			Message: "Assessments are pending successful response delivery and durable reconciliation, not a promise of active memory. Observations are temporary evidence, not active durable facts. Rejected items were not staged.",
		}
		if hasRetryable {
			response.RequiredAction = "Retry only retryable rejected items once using corrected arguments grounded in the same current user message. Do not resubmit staged items or claim rejected items were saved."
		} else if rejected > 0 {
			response.RequiredAction = "Do not retry or claim rejected items were saved."
		}
		content, err := json.Marshal(response)
		if err != nil {
			return governance.Result{}, err
		}
		outcome := governance.OutcomeProductive
		if len(staged) == 0 {
			outcome = governance.OutcomeUnproductive
		}
		requestLog(log, ctx).Info("agent.tool.user_memory.staged", "staged foreground user memory", config.F("tool_name", toolnames.UserMemorySave), config.F("staged_count", len(staged)), config.F("rejected_count", rejected))
		return governance.Result{Content: string(content), Outcome: outcome, ReasonCode: map[bool]string{true: "", false: "policy_rejected"}[len(staged) > 0]}, nil
	}
}

func foregroundClaimSlotPrefixes(category string) []string {
	switch policy.Category(strings.TrimSpace(category)) {
	case policy.CategoryIdentity:
		return []string{"identity."}
	case policy.CategoryCommunicationPreferences:
		return []string{"communication."}
	case policy.CategoryDurablePreferences:
		return []string{"preference.", "durable."}
	case policy.CategoryProjects:
		return []string{"project."}
	case policy.CategoryRelationships:
		return []string{"relationship."}
	case policy.CategoryEnvironment:
		return []string{"environment."}
	case policy.CategoryNotes:
		return []string{"notes."}
	default:
		return nil
	}
}

func foregroundMemoryRejection(reason string) (string, string, bool) {
	switch reason {
	case "semantic claim slot is incompatible with memory category":
		return "invalid_claim_slot", reason, true
	case "evidence is not an exact quote from normalized source user text":
		return "evidence_not_exact", reason, true
	case "user-statement evidence must begin with a direct first-person marker", "user-statement evidence lacks a meaningful first-person fact":
		return "evidence_not_direct", reason, true
	case "direct fact statement must be a concise third-person user statement":
		return "invalid_statement", reason, true
	case "direct fact statement is not lexically grounded in unambiguous exact evidence":
		return "statement_not_grounded", reason, true
	case "claim value is not lexically grounded in exact evidence":
		return "claim_value_not_grounded", reason, true
	case "explicit mode requires an exact remember phrase containing the evidence":
		return "explicit_evidence_mismatch", reason, true
	case "credential material cannot become user memory":
		return "credential_material", reason, false
	case "instruction-like content cannot become user memory", "instruction, policy, authorization, or capability content cannot become user memory":
		return "disallowed_instruction", reason, false
	case "external facts cannot become tenant memory", "evidence describes a third party rather than the user", "publicly attributed content is not a private user fact":
		return "not_private_user_fact", reason, false
	case "hypothetical and quoted content is not user memory", "quoted or reported speech is not a direct user fact", "hypothetical or conditional content is not user memory":
		return "non_asserted_content", reason, false
	case "interrogative, negative, obsolete, or uncertain evidence is not a direct user fact":
		return "not_current_positive_fact", reason, false
	default:
		return "policy_rejected", "candidate did not satisfy user-memory policy", false
	}
}

func decodeForegroundMemorySave(args map[string]interface{}) ([]foregroundMemorySaveItem, error) {
	if len(args) != 1 || args["memories"] == nil {
		return nil, fmt.Errorf("strict arguments require only memories")
	}
	rawItems, ok := args["memories"].([]interface{})
	if !ok || len(rawItems) < 1 || len(rawItems) > requestctx.MaxStagedMemoryCandidates {
		return nil, fmt.Errorf("memories must contain one to five items")
	}
	items := make([]foregroundMemorySaveItem, 0, len(rawItems))
	for i, raw := range rawItems {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("memories[%d] must be an object", i)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("memories[%d] must contain exactly the required fields", i)
		}
		for _, field := range []string{"statement", "evidence", "category", "claim_slot", "claim_value", "supersedes", "evidence_type", "confidence"} {
			if _, ok := fields[field]; !ok {
				return nil, fmt.Errorf("memories[%d].%s is required", i, field)
			}
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		var item foregroundMemorySaveItem
		if err := decoder.Decode(&item); err != nil {
			return nil, fmt.Errorf("memories[%d]: %w", i, err)
		}
		if item.TargetMemoryID < 0 || item.ExpectedRevision < 0 || (item.TargetMemoryID != 0 && item.ReinforcesMemoryID != 0 && item.TargetMemoryID != item.ReinforcesMemoryID) {
			return nil, fmt.Errorf("memories[%d] contains invalid or conflicting target IDs", i)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("memories[%d] has trailing JSON", i)
		}
		if !validForegroundSaveString(item.Statement, 1000, false) || !validForegroundSaveString(item.Evidence, 1000, false) || !validForegroundSaveString(item.Category, 32, false) || !validForegroundSaveString(item.ClaimSlot, 128, false) || !validForegroundSaveString(item.ClaimValue, 256, false) || !validForegroundSaveString(item.Supersedes, 1000, true) || (item.EvidenceType != "direct_statement" && item.EvidenceType != "model_inference") || math.IsNaN(item.Confidence) || math.IsInf(item.Confidence, 0) || item.Confidence < 0 || item.Confidence > 1 || item.ReinforcesMemoryID < 0 {
			return nil, fmt.Errorf("memories[%d] contains an invalid or out-of-bounds field", i)
		}
		items = append(items, item)
	}
	return items, nil
}

func validForegroundSaveString(value string, maxRunes int, allowEmpty bool) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxRunes && (allowEmpty || strings.TrimSpace(value) != "")
}

// NewSearchHandler returns a Handler for memory search.
func NewSearchHandler(store *memory.Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, err := authenticatedPrincipal(ctx, toolnames.UserMemorySearch)
		if err != nil {
			return governance.Result{}, err
		}
		userID := principal.CanonicalUserID
		limit := intArg(args, "limit", 8)
		query := stringArg(args, "query")
		if strings.TrimSpace(query) == "" {
			entries, err := store.ListMemories(userID, stringArg(args, "scope"), stringArg(args, "category"), limit)
			if err != nil {
				return governance.Result{}, err
			}
			if len(entries) == 0 {
				return governance.Result{Content: "No matching memories found for this user.", Outcome: governance.OutcomeUnproductive, ReasonCode: "no_results"}, nil
			}
			return governance.Result{Content: memory.RenderMarkdown("", entries), Outcome: governance.OutcomeProductive}, nil
		}
		results, stats := store.Recall(ctx, userID, query, memory.RecallRequest{
			Scope: stringArg(args, "scope"), Category: stringArg(args, "category"), TopK: limit, ExplicitSearch: true,
		})
		searchLog := requestLog(log, ctx)
		if stats.LexicalError != nil {
			searchLog.Warn("agent.tool.user_memory.search_lexical_degraded", "user memory search lexical channel degraded", config.F("tool_name", toolnames.UserMemorySearch), config.F("status", "degraded"), config.ErrorField(stats.LexicalError))
		}
		if stats.SemanticError != nil {
			searchLog.Warn("agent.tool.user_memory.search_semantic_degraded", "user memory search semantic channel degraded", config.F("tool_name", toolnames.UserMemorySearch), config.F("status", "degraded"), config.ErrorField(stats.SemanticError))
		}
		if !stats.LexicalAvailable && !stats.SemanticAvailable {
			return governance.Result{}, fmt.Errorf("%s: retrieval indexes unavailable", toolnames.UserMemorySearch)
		}
		if len(results) == 0 {
			if stats.LexicalError != nil || stats.SemanticError != nil {
				return governance.Result{Content: "No matching memories found in the available retrieval channel; recall is partially degraded.", Outcome: governance.OutcomeUnproductive, ReasonCode: "no_results", IsDegraded: true}, nil
			}
			return governance.Result{Content: "No matching memories found for this user.", Outcome: governance.OutcomeUnproductive, ReasonCode: "no_results"}, nil
		}
		store.RecordRecallUsage(ctx, userID, results)
		searchLog.Debug("agent.tool.user_memory.searched", "searched user memory",
			config.F("tool_name", toolnames.UserMemorySearch), config.F("returned_count", len(results)),
			config.F("lexical_candidate_count", stats.LexicalCandidateCount),
			config.F("semantic_candidate_count", stats.SemanticCandidateCount))
		output := memory.RenderDurableMemoryRecall(results, 12000)
		if stats.LexicalError != nil || stats.SemanticError != nil {
			output = "Recall is partially degraded; results come from the available retrieval channel.\n\n" + output
		}
		return governance.Result{Content: output, Outcome: governance.OutcomeProductive, IsDegraded: stats.LexicalError != nil || stats.SemanticError != nil}, nil
	}
}

// NewListHandler returns a Handler for listing active memory.
func NewListHandler(store *memory.Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, err := authenticatedPrincipal(ctx, toolnames.UserMemoryList)
		if err != nil {
			return governance.Result{}, err
		}
		userID := principal.CanonicalUserID
		entries, err := store.ListMemories(userID, stringArg(args, "scope"), stringArg(args, "category"), intArg(args, "limit", 25))
		if err != nil {
			return governance.Result{}, err
		}
		if len(entries) == 0 {
			return governance.Result{Content: "No active memories found for this user.", Outcome: governance.OutcomeUnproductive, ReasonCode: "empty_collection"}, nil
		}
		intro, _ := store.ReadIntro(userID)
		requestLog(log, ctx).Debug("agent.tool.user_memory.listed", "listed user memory", config.F("tool_name", toolnames.UserMemoryList), config.F("returned_count", len(entries)))
		return governance.Result{Content: memory.RenderMarkdown(intro, entries), Outcome: governance.OutcomeProductive}, nil
	}
}

func stringArg(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func intArg(args map[string]interface{}, key string, fallback int) int {
	if args == nil || args[key] == nil {
		return fallback
	}
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}
