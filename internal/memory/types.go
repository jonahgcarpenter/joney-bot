package memory

import (
	"errors"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

const (
	FormationExtractorVersion   = "formation-v4"
	AgentSaveExtractorVersion   = "agent-save-v1"
	DurableModelSubmissionLimit = 3

	FormationPurposeBackgroundPattern = "background_pattern"
	FormationPurposeAgentSave         = "agent_save"
)

// ErrStaleFormationJobLease indicates that the exact claimed lease is no longer live.
var ErrStaleFormationJobLease = errors.New("stale memory formation job lease")

// ErrModelSubmissionBudgetExhausted indicates that a durable model-backed job
// has already reserved every allowed provider submission.
var ErrModelSubmissionBudgetExhausted = errors.New("durable model submission budget exhausted")

// ErrStaleSessionCompactionJobLease indicates that the exact compaction lease is no longer live.
var ErrStaleSessionCompactionJobLease = errors.New("stale session compaction job lease")

// FormationSource identifies the canonical turn and request that formed memory.
type FormationSource struct {
	RequestID         string
	SessionID         string
	SessionGeneration int
	TurnID            int64
	Model             string
	ExtractorVersion  string
}

// CandidateProposal is one validated policy result ready for canonical staging.
type CandidateProposal struct {
	Output               policy.CandidateOutput
	Source               FormationSource
	IdempotencyKey       string
	TargetMemoryID       int64
	SupersedesStatement  string
	RequireCorroboration bool
	// Cardinality is set only by the versioned assessment path; empty retains legacy slot rules.
	Cardinality   string
	Correction    bool
	FormationJob  *FormationJob
	CompactionJob *SessionCompactionJob
}

// FormationCandidate is a persisted memory proposal.
type FormationCandidate struct {
	ID                  int64
	UserID              string
	State               string
	UpdatedAt           time.Time
	Scope               string
	Category            string
	Statement           string
	Evidence            string
	Confidence          float64
	Importance          int
	Provenance          string
	SourceAuthority     string
	Sensitivity         string
	FormationMode       string
	DecisionReason      string
	SourceRequestID     string
	SourceSessionID     string
	SourceGeneration    int
	SourceTurnID        int64
	ExtractionModel     string
	ExtractorVersion    string
	SupersedesMemoryID  int64
	SupersedesStatement string
	PublishedMemoryID   int64
	ExpiresAt           time.Time
	ClaimSlot           string
	ClaimValue          string
}

// FormationJob is one leased post-turn extraction operation.
type FormationJob struct {
	ID                      int64
	UserID                  string
	RequestID               string
	SessionID               string
	SessionGeneration       int
	TurnID                  int64
	Model                   string
	ExtractorVersion        string
	Purpose                 string
	AttemptCount            int
	InvalidOutputRetryCount int
	LastErrorCode           string
	ModelSubmissionCount    int
	CorrectiveErrorCode     string
	LeaseOwner              string
	LeaseUntil              time.Time
}

// StoredSessionTurn identifies an exchange that was actually persisted.
type StoredSessionTurn struct {
	ID         int64
	UserID     string
	SessionID  string
	Generation int
	UserText   string
	// CreatedAt is the trusted source timestamp, not extraction time.
	CreatedAt time.Time
	// AssistantResponse is interpretation context only, never memory evidence.
	AssistantResponse string
}

const (
	// SessionCompactionModelSubmissionLimit permits one initial call and three retries.
	SessionCompactionModelSubmissionLimit = 4
	// SessionCompactionInvalidOutputRetryLimit permits all three retries to correct invalid output.
	SessionCompactionInvalidOutputRetryLimit = 3
)

const (
	maxSummaryNarrativeRunes  = 8000
	maxSummaryArrayItems      = 50
	maxSummaryItemRunes       = 1000
	maxSummaryCandidates      = 20
	maxSummaryStructuredRunes = 16000
	maxSummaryArtifactBytes   = 40000
	maxSummaryCandidateRunes  = 2000
)

// SessionSummary is one immutable structured checkpoint for a session generation.
type SessionSummary struct {
	ID                   int64
	UserID               string
	SessionID            string
	SessionGeneration    int
	CoveredFromTurnID    int64
	CoveredThroughTurnID int64
	Narrative            string
	OpenTasks            []string
	Commitments          []string
	Entities             []string
	Decisions            []string
	TopicTags            []string
	SourceTurnIDs        []int64
}

// SessionCompactionJob is one fixed-range, leased chunk of a stable campaign.
type SessionCompactionJob struct {
	ID int64
	// RequestID is the covered endpoint's persisted origin, loaded at claim time.
	RequestID               string `json:"-"`
	UserID                  string
	SessionID               string
	SessionGeneration       int
	CoveredFromTurnID       int64
	CoveredThroughTurnID    int64
	TargetTurnID            int64
	State                   string
	ArtifactSummaryID       int64
	Model                   string
	GeneratorVersion        string
	AttemptCount            int
	InvalidOutputRetryCount int
	LastErrorCode           string
	ModelSubmissionCount    int
	CorrectiveErrorCode     string
	LeaseOwner              string
	LeaseUntil              time.Time
	AvailableAt             time.Time
}

// DeliveredSessionPromptPressure is the newest delivered completed-request
// pressure snapshot for one active session generation.
type DeliveredSessionPromptPressure struct {
	TurnID int64
	SessionPromptPressure
}

// SessionPromptPressure is the immutable completed-request pressure snapshot
// that becomes planner-visible only after its source turn is delivered.
type SessionPromptPressure struct {
	Tokens  int
	Limit   int
	Version string
}

// SummaryArtifact is the first model-produced structured result saved for a job.
// Its canonical JSON representation is immutable after the first successful save.
type SummaryArtifact struct {
	Narrative        string                        `json:"narrative"`
	OpenTasks        []string                      `json:"open_tasks"`
	Commitments      []string                      `json:"commitments"`
	Entities         []string                      `json:"entities"`
	Decisions        []string                      `json:"decisions"`
	TopicTags        []string                      `json:"topic_tags"`
	GenerationModel  string                        `json:"generation_model"`
	GeneratorVersion string                        `json:"generator_version"`
	Candidates       []CompactionCandidateArtifact `json:"candidates"`
}

// CompactionCandidateArtifact is an untrusted source-turn-specific memory proposal.
type CompactionCandidateArtifact struct {
	SourceTurnID int64   `json:"source_turn_id"`
	Statement    string  `json:"statement"`
	Evidence     string  `json:"evidence"`
	Scope        string  `json:"scope"`
	Category     string  `json:"category"`
	Context      string  `json:"context"`
	Provenance   string  `json:"provenance"`
	Sensitivity  string  `json:"sensitivity"`
	Confidence   float64 `json:"confidence"`
	Importance   int     `json:"importance"`
	TTLDays      int     `json:"ttl_days"`
	Supersedes   string  `json:"supersedes"`
	ClaimSlot    string  `json:"claim_slot"`
	ClaimValue   string  `json:"claim_value"`
}

// CompactionWindow gives a planner chronological delivered turns and the
// unbounded count and newest eligible ID before the first pending delivery.
type CompactionWindow struct {
	Turns        []SessionTurn
	TotalCount   int
	NewestTurnID int64
}

// ActiveSessionScope identifies one currently active tenant session generation.
type ActiveSessionScope struct {
	UserID     string
	SessionID  string
	Generation int
}

const (
	ScopeShortTerm = "short_term"
	ScopeLongTerm  = "long_term"

	StatusActive = "active"
)

// ValidCategories lists supported memory categories in display order.
var ValidCategories = []string{"identity", "communication_preferences", "durable_preferences", "projects", "relationships", "environment", "notes"}

// MemoryEntry is a single short-term or long-term user memory.
type MemoryEntry struct {
	Context          string
	RetiredAt        time.Time
	RetirementReason string
	ID               int64
	// Revision is the canonical optimistic-concurrency token, not an index revision.
	Revision        int64
	UserID          string
	Scope           string
	Category        string
	Statement       string
	Evidence        string
	Confidence      float64
	Importance      int
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ExpiresAt       time.Time
	SupersedesID    int64
	ProvenanceType  string
	SourceAuthority string
	Sensitivity     string
	ClaimSlot       string
	ClaimValue      string
	EvidenceCount   int
	Score           float64
}

// SessionTurn is a completed exchange stored for session continuity.
type SessionTurn struct {
	ID            int64
	SessionID     string
	UserID        string
	Generation    int
	UserText      string
	AssistantText string
	ToolNames     []string
	ToolHistory   ToolHistory
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Score         float64
}
