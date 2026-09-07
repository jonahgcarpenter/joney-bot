// Package requestctx provides context key helpers for passing request-scoped
// metadata through the tool execution pipeline.
package requestctx

import (
	"context"
	"fmt"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

type contextKey string

const (
	requestMetaKey contextKey = "request_meta"
	principalKey   contextKey = "principal"
	toolExposeKey  contextKey = "tool_exposer"
	inputImagesKey contextKey = "input_images"
	memoryStageKey contextKey = "memory_stage"
)

// MaxStagedMemoryCandidates is the foreground per-request staging limit.
const MaxStagedMemoryCandidates = 5

// StagedMemoryCandidate is a prevalidated candidate awaiting successful delivery.
type StagedMemoryCandidate struct {
	CanonicalUserID      string
	Candidate            policy.CandidateOutput
	TargetMemoryID       int64
	SupersedesStatement  string
	Retention            string
	Intent               string
	Context              string
	TTLDays              int
	Cardinality          string
	SourceObservationIDs []int64
	ExpectedRevision     int64
}

// MemoryStageCollector holds foreground candidates for one request. It does not
// publish them; the delivery path owns any later durable write.
type MemoryStageCollector struct {
	mu         sync.Mutex
	candidates []StagedMemoryCandidate
}

// NewMemoryStageCollector creates an empty request-local collector.
func NewMemoryStageCollector() *MemoryStageCollector { return &MemoryStageCollector{} }

// Stage atomically appends a bounded batch of prevalidated candidates.
func (c *MemoryStageCollector) Stage(candidates []StagedMemoryCandidate) error {
	if c == nil {
		return fmt.Errorf("memory stage collector is unavailable")
	}
	if len(candidates) == 0 {
		return fmt.Errorf("memory stage batch must not be empty")
	}
	for _, candidate := range candidates {
		if candidate.CanonicalUserID == "" || candidate.TargetMemoryID < 0 || candidate.ExpectedRevision < 0 || len(candidate.SourceObservationIDs) != 0 || (candidate.Candidate.Approval != policy.ApprovalApproved && candidate.Candidate.Approval != policy.ApprovalProposed) {
			return fmt.Errorf("memory stage candidate is not publishable")
		}
		if candidate.CanonicalUserID != candidates[0].CanonicalUserID {
			return fmt.Errorf("memory stage candidates belong to different canonical users")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.candidates)+len(candidates) > MaxStagedMemoryCandidates {
		return fmt.Errorf("memory stage contains more than %d candidates", MaxStagedMemoryCandidates)
	}
	if len(c.candidates) > 0 {
		for _, candidate := range candidates {
			if candidate.CanonicalUserID != c.candidates[0].CanonicalUserID {
				return fmt.Errorf("memory stage candidate belongs to a different canonical user")
			}
		}
	}
	c.candidates = append(c.candidates, candidates...)
	return nil
}

// Candidates returns a defensive copy of the currently staged candidates.
func (c *MemoryStageCollector) Candidates() []StagedMemoryCandidate {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]StagedMemoryCandidate(nil), c.candidates...)
}

// ToolExposer records tools that should become visible for the active request.
type ToolExposer interface {
	ExposeTools(names []string)
}

// Metadata carries request-scoped fields needed by tools and provider logging.
type Metadata struct {
	RequestID         string
	SessionID         string
	SessionGeneration int
	Model             string
	CurrentUserText   string
	// Group provenance is set only by a trusted gateway runtime. PublicUserText
	// is the inbound text before reply, attachment, or model enrichment.
	GroupGateway      string
	GroupChatID       string
	PublicUserText    string
	Workload          string
	OperationID       string
	ParentOperationID string
	JobID             int64
}

// InputImage is a request-scoped copy of one normalized current-turn image.
type InputImage struct {
	MIMEType string
	Data     string
	Source   string
}

// WithPrincipal returns a copy of ctx with the resolved request actor attached.
func WithPrincipal(ctx context.Context, principal identity.Principal) context.Context {
	return context.WithValue(ctx, principalKey, principal)
}

// PrincipalFromContext extracts the resolved request actor from ctx.
func PrincipalFromContext(ctx context.Context) (identity.Principal, bool) {
	principal, ok := ctx.Value(principalKey).(identity.Principal)
	return principal, ok
}

// WithMetadata returns a copy of ctx with request metadata attached.
func WithMetadata(ctx context.Context, meta Metadata) context.Context {
	return context.WithValue(ctx, requestMetaKey, meta)
}

// MetadataFromContext extracts request metadata from ctx.
func MetadataFromContext(ctx context.Context) Metadata {
	meta, _ := ctx.Value(requestMetaKey).(Metadata)
	return meta
}

// WithToolExposer returns a copy of ctx with the active request's tool exposer attached.
func WithToolExposer(ctx context.Context, exposer ToolExposer) context.Context {
	return context.WithValue(ctx, toolExposeKey, exposer)
}

// ToolExposerFromContext extracts the active request's tool exposer from ctx.
func ToolExposerFromContext(ctx context.Context) ToolExposer {
	exposer, _ := ctx.Value(toolExposeKey).(ToolExposer)
	return exposer
}

// WithInputImages attaches a defensive copy of current-turn images to ctx.
func WithInputImages(ctx context.Context, images []InputImage) context.Context {
	return context.WithValue(ctx, inputImagesKey, append([]InputImage(nil), images...))
}

// InputImagesFromContext returns a defensive copy of current-turn images.
func InputImagesFromContext(ctx context.Context) []InputImage {
	images, _ := ctx.Value(inputImagesKey).([]InputImage)
	return append([]InputImage(nil), images...)
}

// WithMemoryStageCollector attaches the request's foreground memory collector.
func WithMemoryStageCollector(ctx context.Context, collector *MemoryStageCollector) context.Context {
	return context.WithValue(ctx, memoryStageKey, collector)
}

// MemoryStageCollectorFromContext returns the request's foreground memory collector.
func MemoryStageCollectorFromContext(ctx context.Context) *MemoryStageCollector {
	collector, _ := ctx.Value(memoryStageKey).(*MemoryStageCollector)
	return collector
}
