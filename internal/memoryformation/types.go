// Package memoryformation validates and classifies proposed user memories.
// It is pure: it performs no storage, network, or model operations.
package memoryformation

import "time"

// Provenance describes where a candidate's content originated.
type Provenance string

const (
	ProvenanceUserStatement  Provenance = "user_statement"
	ProvenanceModelInference Provenance = "model_inference"
	ProvenanceThirdParty     Provenance = "third_party"
	ProvenancePublicSource   Provenance = "public_source"
	ProvenanceToolOutput     Provenance = "tool_output"
)

// SourceAuthority describes the authority actually granted to a source.
type SourceAuthority string

const (
	AuthorityUserDirect SourceAuthority = "user_direct"
	AuthorityModel      SourceAuthority = "model"
	AuthorityThirdParty SourceAuthority = "third_party"
	AuthorityPublic     SourceAuthority = "public"
	AuthorityTool       SourceAuthority = "tool"
)

// Sensitivity describes the policy sensitivity of a candidate.
type Sensitivity string

const (
	SensitivityLow                   Sensitivity = "low"
	SensitivityIdentityOrContact     Sensitivity = "identity_or_contact"
	SensitivityHighImpactInteraction Sensitivity = "high_impact_interaction"
)

// FormationMode identifies the candidate-producing workflow. Agent-save and
// background-pattern candidates are model-assessed and receive structural,
// linkage, metadata, and threshold checks without legacy semantic heuristics.
type FormationMode string

const (
	ModeAutomaticExtraction     FormationMode = "automatic_extraction"
	ModePreCompactionExtraction FormationMode = "pre_compaction_extraction"
	ModeExplicitRemember        FormationMode = "explicit_remember"
	ModeAgentSave               FormationMode = "agent_save"
	ModeBackgroundPattern       FormationMode = "background_pattern"
)

// Approval is the candidate's resulting approval state.
type Approval string

const (
	ApprovalProposed Approval = "proposed"
	ApprovalApproved Approval = "approved"
	ApprovalRejected Approval = "rejected"
)

// PolicyDecision is the action allowed by formation policy.
type PolicyDecision string

const (
	DecisionAutomatic      PolicyDecision = "automatic"
	DecisionInferredActive PolicyDecision = "inferred_active"
	DecisionShortTerm      PolicyDecision = "short_term"
	DecisionProposed       PolicyDecision = "proposed"
	DecisionDisallowed     PolicyDecision = "disallowed"
)

// Scope is the lifetime class requested for a candidate.
type Scope string

const (
	ScopeShortTerm Scope = "short_term"
	ScopeLongTerm  Scope = "long_term"
)

// Category is a constrained memory category.
type Category string

const (
	CategoryIdentity                 Category = "identity"
	CategoryCommunicationPreferences Category = "communication_preferences"
	CategoryDurablePreferences       Category = "durable_preferences"
	CategoryProjects                 Category = "projects"
	CategoryRelationships            Category = "relationships"
	CategoryEnvironment              Category = "environment"
	CategoryNotes                    Category = "notes"
)

// ContentContext identifies whether source text asserts a fact or merely contains it.
type ContentContext string

const (
	ContextDirectAssertion ContentContext = "direct_assertion"
	ContextTemporaryState  ContentContext = "temporary_task_state"
	ContextHypothetical    ContentContext = "hypothetical"
	ContextQuotation       ContentContext = "quotation"
)

// CandidateInput is untrusted candidate data submitted to policy evaluation.
type CandidateInput struct {
	SourceUserText   string
	Statement        string
	Evidence         string
	Provenance       Provenance
	ClaimedAuthority SourceAuthority
	Sensitivity      Sensitivity
	Mode             FormationMode
	Scope            Scope
	Category         Category
	Context          ContentContext
	Confidence       float64
	Importance       int
	TTL              time.Duration
	ClaimSlot        string
	ClaimValue       string
}

// CandidateOutput contains normalized content and the policy result. Current
// model-assessed workflows validate structure; formation-v4 retains its original policy.
type CandidateOutput struct {
	Statement       string
	Evidence        string
	Provenance      Provenance
	SourceAuthority SourceAuthority
	Sensitivity     Sensitivity
	Mode            FormationMode
	Scope           Scope
	Category        Category
	Context         ContentContext
	Confidence      float64
	Importance      int
	TTL             time.Duration
	Approval        Approval
	Decision        PolicyDecision
	Reason          string
	ClaimSlot       string
	ClaimValue      string
}
