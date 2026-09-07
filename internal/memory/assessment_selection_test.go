package memory

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestAssessmentReferencesPreferRelevantIndependentOccasions(t *testing.T) {
	input := AssessmentInput{Version: 1, Anchor: StoredSessionTurn{ID: 100, UserText: "Please give the Docker troubleshooting fix first."}}
	for i := int64(1); i <= 20; i++ {
		input.Observations = append(input.Observations, MemoryObservation{ID: i, SourceTurnID: i, Statement: "I like pancakes", ObservedAt: time.Unix(i, 0)})
	}
	for i := int64(21); i <= 28; i++ {
		input.Observations = append(input.Observations, MemoryObservation{ID: i, SourceTurnID: i, Statement: "Docker troubleshooting: fix first", ObservedAt: time.Unix(i, 0)})
	}
	input.Observations = append(input.Observations,
		MemoryObservation{ID: 90, SourceTurnID: 28, Statement: "Docker troubleshooting: fix first", ObservedAt: time.Unix(28, 0)},
		MemoryObservation{ID: 100, SourceTurnID: 100, Statement: "Docker troubleshooting: fix first"},
		MemoryObservation{ID: 101, SourceTurnID: 101, Statement: "Docker troubleshooting: fix first"})
	selectAssessmentReferences(&input)
	if len(input.Observations) != 6 {
		t.Fatalf("selected %d observations", len(input.Observations))
	}
	seen := map[int64]bool{}
	for _, o := range input.Observations {
		if o.SourceTurnID < 21 || o.SourceTurnID >= 100 || seen[o.SourceTurnID] {
			t.Fatalf("irrelevant, repeated, or future source: %+v", o)
		}
		seen[o.SourceTurnID] = true
	}
	if input.Observations[0].ID != 90 {
		t.Fatalf("unstable recency tie: %+v", input.Observations)
	}
}

func TestAssessmentInputCarriesTrustedTimeContextAndStaging(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		seedFormationTurn(t, s, "user", "assessment", fmt.Sprintf("Earlier question %d", i))
	}
	job := assessmentTestJob(t, s, "I prefer tea.")
	input, err := s.FormationAssessmentInput(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if input.Anchor.CreatedAt.IsZero() || input.Anchor.AssistantResponse != "" || len(input.Context) != 2 {
		t.Fatalf("source projection: %+v", input)
	}
	for _, turn := range input.Context {
		if turn.CreatedAt.IsZero() || turn.AssistantResponse == "" || turn.ID >= job.TurnID {
			t.Fatalf("missing trusted interpretation context: %+v", turn)
		}
	}
	again, err := s.FormationAssessmentInput(ctx, job)
	if err != nil || !reflect.DeepEqual(input, again) {
		t.Fatalf("frozen input changed: %+v %v", again, err)
	}
}

func TestAssessmentDifferentObservationClaimCanSupportInference(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	observation := assessmentTestCandidate()
	observation.Retention = "observation"
	observation.ClaimSlot = "preference.today_drink"
	first := assessmentTestJob(t, s, observation.Evidence)
	if _, err := s.ApplyMemoryAssessment(ctx, first, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{observation}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFormationJob(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	second := assessmentTestJob(t, s, "Again, I prefer tea.")
	input, err := s.FormationAssessmentInput(ctx, second)
	if err != nil || len(input.Observations) != 1 {
		t.Fatalf("observation selection: %+v %v", input, err)
	}
	inference := assessmentTestCandidate()
	inference.EvidenceType, inference.Provenance = "model_inference", "model_inference"
	inference.SourceObservationIDs = []int64{input.Observations[0].ID}
	result, err := s.ApplyMemoryAssessment(ctx, second, AssessmentBatch{Version: 1, Items: []ForegroundMemoryCandidate{inference}})
	if err != nil || result.PublishedCount != 1 {
		t.Fatalf("cross-claim evidence: %+v %v", result, err)
	}
}
