package governance

import "testing"

func testPolicy() ToolPolicy {
	return ToolPolicy{MaxExecutions: 3, MaxFailures: 2, MaxUnproductive: 2, BlockDuplicates: true}
}

func testGlobal() GlobalPolicy {
	return GlobalPolicy{MaxExecutions: 4, MaxToolIterations: 2}
}

func TestDefaultGlobalPolicy(t *testing.T) {
	policy := DefaultGlobalPolicy()
	if policy != (GlobalPolicy{MaxExecutions: 50, MaxToolIterations: 30}) {
		t.Fatalf("default policy = %+v", policy)
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintCanonicalizesMapOrder(t *testing.T) {
	a, err := Fingerprint("test.tool", map[string]interface{}{"a": 1, "b": "two"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint("test.tool", map[string]interface{}{"b": "two", "a": 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("fingerprints differ: %q != %q", a, b)
	}
}

func TestPoliciesValidateSafetyLimits(t *testing.T) {
	if err := testPolicy().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := testGlobal().Validate(); err != nil {
		t.Fatal(err)
	}
	invalidTool := testPolicy()
	invalidTool.MaxUnproductive = -1
	if err := invalidTool.Validate(); err == nil {
		t.Fatal("negative unproductive limit was accepted")
	}
	unlimited := ToolPolicy{}
	if err := unlimited.Validate(); err != nil {
		t.Fatalf("zero-value unlimited policy was rejected: %v", err)
	}
	invalidGlobal := testGlobal()
	invalidGlobal.MaxExecutions = 0
	if err := invalidGlobal.Validate(); err == nil {
		t.Fatal("zero global execution limit was accepted")
	}
}

func TestGovernorBlocksDuplicatesWithoutConsumingExecution(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	decision := g.BeforeExecution("test.tool", map[string]interface{}{"q": "one"}, policy, true)
	if !decision.Allowed {
		t.Fatalf("first decision = %+v", decision)
	}
	g.RecordResult("test.tool", decision, Result{Outcome: OutcomeProductive}, nil)
	decision = g.BeforeExecution("test.tool", map[string]interface{}{"q": "one"}, policy, true)
	if decision.Allowed || decision.ReasonCode != ReasonDuplicate {
		t.Fatalf("duplicate decision = %+v", decision)
	}
	stats := g.Stats("test.tool")
	if stats.Attempts != 2 || stats.Executions != 1 || stats.Duplicates != 1 || g.TotalExecutions() != 1 {
		t.Fatalf("unexpected stats: %+v total=%d", stats, g.TotalExecutions())
	}
}

func TestGovernorAllowsExactRetryAfterExecutionFailure(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	args := map[string]interface{}{"q": "same"}

	first := g.BeforeExecution("test.tool", args, policy, true)
	if !first.Allowed {
		t.Fatalf("first decision = %+v", first)
	}
	g.RecordResult("test.tool", first, Result{}, assertError{})

	retry := g.BeforeExecution("test.tool", args, policy, true)
	if !retry.Allowed {
		t.Fatalf("retry decision = %+v", retry)
	}
	g.RecordResult("test.tool", retry, Result{Outcome: OutcomeProductive}, nil)

	duplicate := g.BeforeExecution("test.tool", args, policy, true)
	if duplicate.Allowed || duplicate.ReasonCode != ReasonDuplicate {
		t.Fatalf("post-success duplicate decision = %+v", duplicate)
	}
}

func TestGovernorZeroLimitsDoNotRetireTool(t *testing.T) {
	g := New(GlobalPolicy{MaxExecutions: 20, MaxToolIterations: 20})
	policy := ToolPolicy{BlockDuplicates: true}
	for i := 0; i < 10; i++ {
		decision := g.BeforeExecution("unlimited.tool", map[string]interface{}{"n": i}, policy, true)
		if !decision.Allowed {
			t.Fatalf("call %d blocked: %+v", i, decision)
		}
		g.RecordResult("unlimited.tool", decision, Result{Outcome: OutcomeUnproductive}, assertError{})
	}
	if g.IsToolRetired("unlimited.tool", policy) {
		t.Fatal("zero-limit tool was retired")
	}
	if g.GlobalStopReason() != "" || g.Stats("unlimited.tool").Failures != 10 {
		t.Fatalf("failures must remain statistics, not a global ceiling: %+v", g.Stats("unlimited.tool"))
	}
}

func TestGovernorRetiresOnlyExhaustedTool(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	for i := 0; i < policy.MaxUnproductive; i++ {
		decision := g.BeforeExecution("empty.tool", map[string]interface{}{"n": i}, policy, true)
		if !decision.Allowed {
			t.Fatalf("call %d blocked: %+v", i, decision)
		}
		g.RecordResult("empty.tool", decision, Result{Outcome: OutcomeUnproductive}, nil)
	}
	if !g.IsToolRetired("empty.tool", policy) {
		t.Fatal("empty tool was not retired")
	}
	if g.IsToolRetired("other.tool", policy) {
		t.Fatal("unrelated tool was retired")
	}
}

func TestGovernorFailuresRetireOnlyExhaustedTool(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	badDecision := g.BeforeExecution("bad.tool", map[string]interface{}{"n": 1}, policy, true)
	if !badDecision.Allowed {
		t.Fatal(badDecision)
	}
	g.RecordResult("bad.tool", badDecision, Result{}, assertError{})
	retry := g.BeforeExecution("bad.tool", map[string]interface{}{"n": 1}, policy, true)
	if !retry.Allowed {
		t.Fatal(retry)
	}
	g.RecordResult("bad.tool", retry, Result{}, assertError{})
	if !g.IsToolRetired("bad.tool", policy) || g.GlobalStopReason() != "" {
		t.Fatal("per-tool failures did not remain isolated")
	}
	if blocked := g.BeforeExecution("bad.tool", nil, policy, true); blocked.Allowed || blocked.ReasonCode != ReasonToolFailures {
		t.Fatalf("retired tool decision = %+v", blocked)
	}
	goodDecision := g.BeforeExecution("good.tool", map[string]interface{}{"n": 1}, policy, true)
	if !goodDecision.Allowed {
		t.Fatal(goodDecision)
	}
	g.RecordResult("good.tool", goodDecision, Result{Outcome: OutcomeProductive}, nil)
	if g.Stats("bad.tool").Failures != 2 {
		t.Fatal("per-tool failure count was reset")
	}
}

func TestGovernorEnforcesGlobalExecutionAndIterationLimits(t *testing.T) {
	global := testGlobal()
	global.MaxExecutions = 1
	g := New(global)
	policy := testPolicy()
	if decision := g.BeginToolIteration(); !decision.Allowed {
		t.Fatal(decision)
	}
	if decision := g.BeforeExecution("one", nil, policy, true); !decision.Allowed {
		t.Fatal(decision)
	}
	if reason := g.GlobalStopReason(); reason != ReasonGlobalLimit {
		t.Fatalf("global stop reason = %q", reason)
	}
	if decision := g.BeforeExecution("two", nil, policy, true); decision.Allowed || decision.ReasonCode != ReasonGlobalLimit {
		t.Fatalf("post-limit decision = %+v", decision)
	}

	g = New(testGlobal())
	if decision := g.BeginToolIteration(); !decision.Allowed {
		t.Fatal(decision)
	}
	if decision := g.BeginToolIteration(); !decision.Allowed {
		t.Fatal(decision)
	}
	if decision := g.BeginToolIteration(); decision.Allowed || decision.ReasonCode != ReasonIterationLimit {
		t.Fatalf("iteration decision = %+v", decision)
	}
}

type assertError struct{}

func (assertError) Error() string { return "failure" }
