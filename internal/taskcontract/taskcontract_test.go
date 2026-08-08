package taskcontract

import (
	"strings"
	"testing"

	"reasonix/internal/evidence"
	"reasonix/internal/taskintent"
)

func TestNewClassifiesWithExistingHeuristics(t *testing.T) {
	if c := New("fix the bug in utils.py"); c.Intent != taskintent.Mutation {
		t.Fatalf("intent = %v, want Mutation", c.Intent)
	}
	if c := New("what does this function do?"); c.Intent == taskintent.Mutation {
		t.Fatalf("advisory question misclassified as mutation")
	}
}

func TestMergeSignalsRatchetsRiskAndScope(t *testing.T) {
	c := New("refactor the parser across reader.py and writer.py")
	c.MergeSignals(Signals{MediumRisk: true, MultiFile: true, Paths: []string{"reader.py"}})
	c.MergeSignals(Signals{HighRisk: true, Paths: []string{"writer.py", "reader.py"}})
	if c.Risk != RiskHigh {
		t.Fatalf("risk = %v, want high (ratchet up)", c.Risk)
	}
	c.MergeSignals(Signals{MediumRisk: true})
	if c.Risk != RiskHigh {
		t.Fatalf("risk downgraded to %v — must only ratchet up", c.Risk)
	}
	if !c.Scope.MultiFile || len(c.Scope.Paths) != 2 {
		t.Fatalf("scope = %+v, want multi-file with 2 deduped paths", c.Scope)
	}
}

func TestChecksSatisfyFromReceipts(t *testing.T) {
	c := New("fix add and verify with go test")
	c.AddCheck("go test ./...")
	c.AddCheck("")

	c.Observe(evidence.Receipt{ToolName: "bash", Command: "go test ./...", Success: false})
	if c.Checks[0].Status != Failed {
		t.Fatalf("failed run must mark the check failed, got %v", c.Checks[0].Status)
	}
	c.Observe(evidence.Receipt{ToolName: "bash", Command: "go test ./...", Success: true})
	if c.Checks[0].Status != Satisfied || c.Checks[1].Status != Satisfied {
		t.Fatalf("checks = %+v, want both satisfied (go test is a verification)", c.Checks)
	}
	if c.Epoch() != 0 {
		t.Fatalf("epoch = %d, want 0 (verifications never advance the mutation epoch)", c.Epoch())
	}
	if got := c.Checks[0].Evidence[1]; got.Kind != EvidenceVerification || got.MutationEpoch != 0 || !got.Success {
		t.Fatalf("evidence ref = %+v", got)
	}
}

func TestRequirementsResolveExplicitly(t *testing.T) {
	c := New("implement the ledger")
	c.AddRequirement("r1", "balances.json written", true)
	c.AddRequirement("r2", "nice-to-have docs", false)
	c.AddCheck("")

	if c.Complete() {
		t.Fatal("nothing satisfied yet")
	}
	c.Observe(evidence.Receipt{ToolName: "write_file", Write: true, Mutation: true, Success: true})
	c.Observe(evidence.Receipt{ToolName: "bash", Command: "go test ./...", Success: true})
	if c.Complete() {
		t.Fatal("required requirement still pending; checks alone must not complete the contract")
	}
	if !c.Resolve("r1", Satisfied, EvidenceRef{Kind: EvidenceMutation, MutationEpoch: 1, Source: "write_file", Success: true}) {
		t.Fatal("resolve r1")
	}
	if !c.Complete() {
		t.Fatalf("contract should be complete; outstanding: %v", c.Outstanding())
	}
	if c.Resolve("missing", Satisfied) {
		t.Fatal("unknown requirement must not resolve")
	}
}

func TestOutstandingListsBlockersOnly(t *testing.T) {
	c := New("do the thing")
	c.AddRequirement("r1", "must happen", true)
	c.AddRequirement("r2", "optional", false)
	c.AddCheck("go vet ./...")
	got := c.Outstanding()
	if len(got) != 2 {
		t.Fatalf("outstanding = %v, want required requirement + check only", got)
	}
}

func TestAtomicContractCompletesFromOneMutation(t *testing.T) {
	c := Atomic("fix typo in README.md")
	if c.Intent != taskintent.Mutation {
		t.Fatalf("intent = %v, want Mutation", c.Intent)
	}
	if !c.Trivial() {
		t.Fatal("atomic contract must route trivial (executor-only, no arbiters)")
	}
	if c.Complete() {
		t.Fatal("nothing happened yet")
	}
	c.Observe(evidence.Receipt{ToolName: "read_file", Read: true, Success: true})
	if c.Complete() {
		t.Fatal("a read must not complete a mutation contract")
	}
	c.Observe(evidence.Receipt{ToolName: "edit_file", Mutation: true, Write: true, Success: true, Paths: []string{"README.md"}})
	if !c.Complete() {
		t.Fatalf("one successful mutation must complete it; outstanding: %v", c.Outstanding())
	}
	if c.Requirements[0].Status != Satisfied || len(c.Requirements[0].Evidence) != 1 {
		t.Fatalf("r1 = %+v, want auto-satisfied with the mutation ref", c.Requirements[0])
	}
}

func TestMutationCheckHonorsScopePaths(t *testing.T) {
	c := Atomic("fix typo in README.md")
	c.MergeSignals(Signals{Anchored: true, Paths: []string{"README.md"}})
	c.Observe(evidence.Receipt{ToolName: "write_file", Mutation: true, Success: true, Paths: []string{"other.txt"}})
	if c.Checks[0].Status == Satisfied {
		t.Fatal("mutation outside the scoped paths must not satisfy the check")
	}
	c.Observe(evidence.Receipt{ToolName: "write_file", Mutation: true, Success: true, Paths: []string{"README.md"}})
	if c.Checks[0].Status != Satisfied {
		t.Fatal("scoped mutation must satisfy the check")
	}
}

func TestTrivialRejectsComplexContracts(t *testing.T) {
	c := Atomic("fix typo")
	c.MergeSignals(Signals{HighRisk: true})
	if c.Trivial() {
		t.Fatal("high risk is never trivial")
	}
	c2 := New("refactor everything")
	c2.MergeSignals(Signals{MultiFile: true})
	if c2.Trivial() {
		t.Fatal("multi-file is never trivial")
	}
}

func TestFromPlanBuildsContractFromPlannerOutput(t *testing.T) {
	c := FromPlan("fix stale cache invalidation", PlanFacts{
		AcceptanceCriteria: []string{"fix stale cache invalidation"},
		Regressions:        []string{"existing cache tests continue passing"},
		Verifications:      []string{"go test ./internal/cache/", ""},
		Risky:              true,
		Touchpoints:        []string{"cache.go", "invalidate.go"},
	})
	if len(c.Requirements) != 2 || c.Requirements[0].Kind != "behavior" || c.Requirements[1].Kind != "regression" {
		t.Fatalf("requirements = %+v", c.Requirements)
	}
	if c.Requirements[1].ID != "g1" || !c.Requirements[1].Required {
		t.Fatalf("regression requirement = %+v", c.Requirements[1])
	}
	if len(c.Checks) != 2 || c.Risk != RiskMedium || !c.Scope.MultiFile || len(c.Scope.Paths) != 2 {
		t.Fatalf("checks=%d risk=%v scope=%+v", len(c.Checks), c.Risk, c.Scope)
	}
	if c.Trivial() {
		t.Fatal("a risky multi-file plan contract must not route trivial")
	}
}

func TestExecutionViewIsAViewNotAParallelDescription(t *testing.T) {
	c := FromPlan("fix it", PlanFacts{
		AcceptanceCriteria: []string{"behavior fixed"},
		Verifications:      []string{"go test ./..."},
	})
	todos := c.ExecutionView()
	if len(todos) != 2 || todos[0].Content != "behavior fixed" || todos[1].Content != "verify: go test ./..." {
		t.Fatalf("view = %+v", todos)
	}
	if todos[0].Status != "pending" {
		t.Fatalf("unsatisfied entries must render pending, got %q", todos[0].Status)
	}
	c.Observe(evidence.Receipt{ToolName: "bash", Command: "go test ./...", Success: true})
	c.Resolve("r1", Satisfied)
	for i, todo := range c.ExecutionView() {
		if todo.Status != "completed" {
			t.Fatalf("todo %d should render completed after satisfaction: %+v", i, todo)
		}
	}
	atomicView := Atomic("fix typo").ExecutionView()
	if len(atomicView) != 2 || atomicView[1].Content != "apply the change" {
		t.Fatalf("atomic view = %+v", atomicView)
	}
}

func TestMutationStalesVerificationEvidence(t *testing.T) {
	c := New("fix foo")
	c.AddCheck("go test ./foo/")
	c.AddRequirement("r2", "no regression", true)

	// epoch 0→1: patch foo.go; epoch 1: go test PASS proves it.
	c.Observe(evidence.Receipt{ToolName: "edit_file", Mutation: true, Success: true, Paths: []string{"foo.go"}})
	c.Observe(evidence.Receipt{ToolName: "bash", Command: "go test ./foo/", Success: true})
	c.Resolve("r2", Satisfied, EvidenceRef{Kind: EvidenceVerification, MutationEpoch: c.Epoch(), Source: "bash", Success: true})
	if !c.Complete() {
		t.Fatalf("satisfied at epoch %d; outstanding: %v", c.Epoch(), c.Outstanding())
	}

	// epoch 2: foo.go changes again — the epoch-1 test proves nothing now.
	c.Observe(evidence.Receipt{ToolName: "edit_file", Mutation: true, Success: true, Paths: []string{"foo.go"}})
	if c.Checks[0].Status != Stale {
		t.Fatalf("check status = %v, want Stale after a newer mutation", c.Checks[0].Status)
	}
	if c.Requirements[0].Status != Stale {
		t.Fatalf("verification-backed requirement must stale, got %v", c.Requirements[0].Status)
	}
	if c.Complete() {
		t.Fatal("stale proof must not count as complete")
	}
	found := false
	for _, item := range c.Outstanding() {
		if strings.Contains(item, "stale: re-verify") {
			found = true
		}
	}
	if !found {
		t.Fatalf("outstanding must name the stale check: %v", c.Outstanding())
	}

	// Re-verify at epoch 2 → satisfied again.
	c.Observe(evidence.Receipt{ToolName: "bash", Command: "go test ./foo/", Success: true})
	if c.Checks[0].Status != Satisfied {
		t.Fatalf("re-verification must re-satisfy, got %v", c.Checks[0].Status)
	}
}

func TestMutationEvidenceNeverStales(t *testing.T) {
	c := Atomic("fix typo in README.md")
	c.Observe(evidence.Receipt{ToolName: "edit_file", Mutation: true, Success: true, Paths: []string{"README.md"}})
	if !c.Complete() {
		t.Fatal("atomic contract should complete on the mutation")
	}
	c.Observe(evidence.Receipt{ToolName: "write_file", Mutation: true, Success: true, Paths: []string{"notes.txt"}})
	if !c.Complete() {
		t.Fatalf("mutation-backed satisfaction must survive later mutations; graph:\n%s", c.Graph())
	}
}

func TestGraphRendersEvidenceTree(t *testing.T) {
	c := New("fix the bug")
	c.AddRequirement("r1", "bug fixed", true)
	c.AddCheck("go test ./...")
	c.Observe(evidence.Receipt{ToolName: "edit_file", Mutation: true, Success: true})
	c.Observe(evidence.Receipt{ToolName: "bash", Command: "go test ./...", Success: true})
	c.Resolve("r1", Satisfied, EvidenceRef{Kind: EvidenceMutation, MutationEpoch: 1, Source: "edit_file", Success: true})
	c.Observe(evidence.Receipt{ToolName: "edit_file", Mutation: true, Success: true})

	graph := c.Graph()
	for _, want := range []string{
		"r1 bug fixed [satisfied]",
		"└── E1 mutation@1 edit_file success=true",
		"check go test ./... [STALE]",
		"verification@1 bash success=true (stale)",
	} {
		if !strings.Contains(graph, want) {
			t.Fatalf("graph missing %q:\n%s", want, graph)
		}
	}
}
