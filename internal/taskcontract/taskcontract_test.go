package taskcontract

import (
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
	if c.Epoch() != 2 {
		t.Fatalf("epoch = %d, want 2", c.Epoch())
	}
	if got := c.Checks[0].Evidence[1]; got.Kind != EvidenceVerification || got.MutationEpoch != 2 || !got.Success {
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
