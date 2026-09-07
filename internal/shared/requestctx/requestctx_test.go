package requestctx

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

type testExposer struct{ names []string }

func (e *testExposer) ExposeTools(names []string) { e.names = append(e.names, names...) }

func TestPrincipalAndMetadataRoundTrip(t *testing.T) {
	principal := identity.Principal{CanonicalUserID: "sender-1", Gateway: "homeassistant", ExternalID: "external-1", Assurance: identity.AssuranceSelfAsserted}
	ctx := WithPrincipal(context.Background(), principal)
	ctx = WithMetadata(ctx, Metadata{RequestID: "req-1", SessionID: "session-1", SessionGeneration: 3})

	meta := MetadataFromContext(ctx)
	gotPrincipal, ok := PrincipalFromContext(ctx)
	if !ok || gotPrincipal != principal || meta.RequestID != "req-1" || meta.SessionID != "session-1" || meta.SessionGeneration != 3 {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
}

func TestMemoryStageCollectorBoundsAndCopiesCandidates(t *testing.T) {
	collector := NewMemoryStageCollector()
	ctx := WithMemoryStageCollector(context.Background(), collector)
	gotCollector := MemoryStageCollectorFromContext(ctx)
	approved := policy.CandidateOutput{Approval: policy.ApprovalApproved, Statement: "The user prefers tea."}
	if err := gotCollector.Stage([]StagedMemoryCandidate{{CanonicalUserID: "usr_1", Candidate: approved}}); err != nil {
		t.Fatal(err)
	}
	got := collector.Candidates()
	got[0].Candidate.Statement = "mutated"
	if collector.Candidates()[0].Candidate.Statement != "The user prefers tea." {
		t.Fatal("Candidates returned shared slice storage")
	}
	proposed := approved
	proposed.Approval = policy.ApprovalProposed
	if err := collector.Stage([]StagedMemoryCandidate{{CanonicalUserID: "usr_1", Candidate: proposed, TargetMemoryID: 9}, {CanonicalUserID: "usr_1", Candidate: approved}, {CanonicalUserID: "usr_1", Candidate: approved}, {CanonicalUserID: "usr_1", Candidate: approved}}); err != nil {
		t.Fatalf("five-candidate request limit was not accepted: %v", err)
	}
	if err := collector.Stage([]StagedMemoryCandidate{{CanonicalUserID: "usr_1", Candidate: approved}}); err == nil {
		t.Fatal("expected request-local candidate limit")
	}
	if len(collector.Candidates()) != MaxStagedMemoryCandidates {
		t.Fatal("failed batch partially mutated collector")
	}
	rejected := approved
	rejected.Approval = policy.ApprovalRejected
	if err := NewMemoryStageCollector().Stage([]StagedMemoryCandidate{{CanonicalUserID: "usr_1", Candidate: rejected}}); err == nil {
		t.Fatal("rejected candidate was accepted")
	}
	other := NewMemoryStageCollector()
	if err := other.Stage([]StagedMemoryCandidate{{CanonicalUserID: "usr_1", Candidate: approved}, {CanonicalUserID: "usr_2", Candidate: approved}}); err == nil {
		t.Fatal("expected mixed-tenant batch rejection")
	}
}

func TestToolExposerRoundTrip(t *testing.T) {
	exposer := &testExposer{}
	ctx := WithToolExposer(context.Background(), exposer)
	got := ToolExposerFromContext(ctx)
	if got == nil {
		t.Fatal("expected exposer")
	}
	got.ExposeTools([]string{"tool"})
	if len(exposer.names) != 1 || exposer.names[0] != "tool" {
		t.Fatalf("unexpected exposer calls: %+v", exposer.names)
	}
}

func TestMemoryStageRetainsAssessmentAndRejectsBackgroundReferences(t *testing.T) {
	c := StagedMemoryCandidate{CanonicalUserID: "user", Candidate: policy.CandidateOutput{Approval: policy.ApprovalProposed}, Retention: "observation", Intent: "correction", Context: "task only", TTLDays: 7, Cardinality: "multiple", TargetMemoryID: 17, ExpectedRevision: 4}
	collector := NewMemoryStageCollector()
	if err := collector.Stage([]StagedMemoryCandidate{c}); err != nil {
		t.Fatal(err)
	}
	got := collector.Candidates()[0]
	if got.Retention != c.Retention || got.Intent != c.Intent || got.Context != c.Context || got.TTLDays != 7 || got.Cardinality != "multiple" || got.ExpectedRevision != 4 {
		t.Fatalf("lost assessment: %+v", got)
	}
	c.SourceObservationIDs = []int64{1}
	if err := collector.Stage([]StagedMemoryCandidate{c}); err == nil {
		t.Fatal("background references accepted in foreground stage")
	}
	if len(collector.Candidates()) != 1 {
		t.Fatal("rejected batch mutated stage")
	}
}

func TestInputImagesUseDefensiveCopies(t *testing.T) {
	images := []InputImage{{MIMEType: "image/png", Data: "encoded", Source: "source"}}
	ctx := WithInputImages(context.Background(), images)
	images[0].Data = "mutated-input"
	got := InputImagesFromContext(ctx)
	if len(got) != 1 || got[0].Data != "encoded" {
		t.Fatalf("stored images were mutated: %+v", got)
	}
	got[0].Data = "mutated-output"
	if again := InputImagesFromContext(ctx); again[0].Data != "encoded" {
		t.Fatalf("returned images shared context storage: %+v", again)
	}
}
