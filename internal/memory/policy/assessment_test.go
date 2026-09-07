package policy

import (
	"strings"
	"testing"
)

func TestAssessmentClaimIdentityPreservesPunctuation(t *testing.T) {
	seen := map[string]bool{}
	for _, value := range []string{"C", "C++", "C#", "a+b", "a#b", "a_b"} {
		slot, got := NormalizeAssessmentClaimIdentity(CategoryEnvironment, "environment.language", value, "")
		if slot != "environment.language" || seen[got] {
			t.Fatalf("identity collision for %q: %q", value, got)
		}
		seen[got] = true
		_, again := NormalizeAssessmentClaimIdentity(CategoryEnvironment, slot, got, "")
		if got != again {
			t.Fatalf("normalization is not idempotent: %q -> %q", got, again)
		}
	}
	for input, want := range map[string]string{"C plus plus": "c++", "CSharp": "c#", " C++ ": "c++"} {
		_, got := NormalizeAssessmentClaimIdentity(CategoryEnvironment, "environment.language", input, "")
		if got != want {
			t.Fatalf("alias %q = %q, want %q", input, got, want)
		}
	}
	_, legacy := NormalizeClaimIdentity(CategoryEnvironment, "environment.language", "C++", "")
	if legacy != "c" {
		t.Fatalf("legacy normalization changed: %q", legacy)
	}
	long := strings.Repeat("a", 256)
	_, a := NormalizeAssessmentClaimIdentity(CategoryNotes, "notes.fact", long+"+", "")
	_, b := NormalizeAssessmentClaimIdentity(CategoryNotes, "notes.fact", long+"#", "")
	if a == b {
		t.Fatal("oversized identities were truncated into a collision")
	}
}

func TestAssessmentClaimIdentityMatchesLegacyOrdinaryClaims(t *testing.T) {
	for _, category := range []Category{CategoryIdentity, CategoryCommunicationPreferences, CategoryDurablePreferences, CategoryProjects, CategoryRelationships, CategoryEnvironment, CategoryNotes} {
		for _, slot := range []string{"", string(category) + ".fact", "  " + string(category) + ".Home City  ", string(category) + ".home-city", string(category) + ".home_city", string(category) + ".home/city", "..."} {
			for _, value := range []string{"New York", " New\t York\n", "new_york", "New-York", "New/York", "New.York", "New, York!", "", "!!!", "San Francisco", "MIXED_case"} {
				oldSlot, oldValue := NormalizeClaimIdentity(category, slot, value, "The user lives in New York.")
				newSlot, newValue := NormalizeAssessmentClaimIdentity(category, slot, value, "The user lives in New York.")
				if newSlot != oldSlot || newValue != oldValue {
					t.Fatalf("legacy key mismatch category=%s slot=%q value=%q: (%q,%q) != (%q,%q)", category, slot, value, newSlot, newValue, oldSlot, oldValue)
				}
				againSlot, againValue := NormalizeAssessmentClaimIdentity(category, newSlot, newValue, "")
				if againSlot != newSlot || againValue != newValue {
					t.Fatal("ordinary normalization is not idempotent")
				}
			}
		}
	}
	for _, value := range []string{"Arch Linux", "arch-linux", "arch_family", "pacman based linux"} {
		oldSlot, oldValue := NormalizeClaimIdentity(CategoryEnvironment, "environment.linux_distribution", value, "")
		newSlot, newValue := NormalizeAssessmentClaimIdentity(CategoryEnvironment, "environment.linux_distribution", value, "")
		if oldSlot != newSlot || oldValue != newValue {
			t.Fatalf("legacy Arch alias mismatch: %q", value)
		}
	}
	for _, suffix := range []string{"c", "c++", "c#"} {
		slot, _ := NormalizeAssessmentClaimIdentity(CategoryEnvironment, "environment."+suffix, "compiler", "")
		if slot != "environment."+suffix {
			t.Fatalf("significant slot punctuation lost: %q", slot)
		}
	}
}

func TestAssessmentExactEvidenceAndModelSupport(t *testing.T) {
	in := CandidateInput{SourceUserText: "I want\n pancakes now.", Statement: "The user wants pancakes now.", Evidence: "I want\n pancakes now.", Provenance: ProvenanceUserStatement, ClaimedAuthority: AuthorityUserDirect, Sensitivity: SensitivityLow, Mode: ModeAgentSave, Scope: ScopeLongTerm, Category: CategoryNotes, Context: ContextDirectAssertion, Confidence: .99, Importance: 3, ClaimSlot: "notes.desire", ClaimValue: "pancakes"}
	out, err := EvaluateAssessment(in)
	if err != nil || out.Approval != ApprovalApproved || out.Evidence != in.Evidence {
		t.Fatalf("exact assessment: %+v %v", out, err)
	}
	in.Evidence = "I want pancakes now."
	out, err = EvaluateAssessment(in)
	if err != nil || out.Approval != ApprovalRejected {
		t.Fatalf("nonverbatim evidence accepted: %+v %v", out, err)
	}
	in.SourceUserText = strings.Repeat("x", 16001) + in.Evidence
	in.Confidence = .34
	out, err = EvaluateAssessment(in)
	if err != nil || out.Approval != ApprovalProposed {
		t.Fatalf("bounded span in long turn: %+v %v", out, err)
	}
	for _, evidence := range []string{"If this task runs, I might use C++.", "I used to live in Paris.", "I no longer like coffee.", "My colleague said they prefer tea."} {
		in.SourceUserText, in.Evidence, in.Statement, in.Confidence, in.Provenance = evidence, evidence, evidence, .8, ProvenanceModelInference
		out, err = EvaluateAssessment(in)
		if err != nil || out.Approval == ApprovalRejected {
			t.Fatalf("blanket semantic rejection: %+v %v", out, err)
		}
	}
	if AssessmentClaimSlotCompatible(CategoryNotes, "identity.name") || AssessmentClaimSlotCompatible(Category("unknown"), "unknown.fact") {
		t.Fatal("invalid category prefix accepted")
	}
}
