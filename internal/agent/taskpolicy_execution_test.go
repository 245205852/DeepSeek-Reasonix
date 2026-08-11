package agent

import (
	"context"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/provider"
	"reasonix/internal/taskpolicy"
	"reasonix/internal/tool"
)

func TestTaskPolicyEnforcesVerificationAllowlist(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Add(fakeTool{name: "bash", readOnly: true})
	a := New(&scriptedProvider{name: "p"}, reg, NewSession("sys"), Options{}, event.Discard)
	a.turnPolicy = taskpolicy.Derive(taskpolicy.Input{Raw: "fix it; only run go test ./internal/parser"})
	a.turnPolicySet = true

	blocked := a.executeOne(context.Background(), provider.ToolCall{Name: "bash", Arguments: `{"command":"npm test"}`})
	if !blocked.blocked || !strings.Contains(blocked.errMsg, "allowlist") {
		t.Fatalf("npm test outcome = %+v, want allowlist block", blocked)
	}
	allowed := a.executeOne(context.Background(), provider.ToolCall{Name: "bash", Arguments: `{"command":"go test ./internal/parser"}`})
	if allowed.blocked || allowed.errMsg != "" {
		t.Fatalf("allowed go test outcome = %+v", allowed)
	}
}

func TestTaskPolicyBlocksDisallowedExploreSubagent(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Add(fakeTool{name: "explore", readOnly: true})
	a := New(&scriptedProvider{name: "p"}, reg, NewSession("sys"), Options{}, event.Discard)
	a.turnPolicy = taskpolicy.TaskPolicy{AllowExploreSubagent: false}
	a.turnPolicySet = true

	got := a.executeOne(context.Background(), provider.ToolCall{Name: "explore", Arguments: `{}`})
	if !got.blocked || !strings.Contains(got.errMsg, "exploration sub-agent") {
		t.Fatalf("explore outcome = %+v, want task-policy block", got)
	}
}

func TestTaskPolicyRequiresPostMutationVerification(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Add(fakeTool{name: "bash", readOnly: true})
	writer := evidence.Receipt{ToolName: "write_file", Success: true, Write: true, Mutation: true}
	check := evidence.Receipt{ToolName: "bash", Success: true, Command: "go test ./..."}
	a := &Agent{
		evidence:      readinessLedger(check, writer),
		tools:         reg,
		turnPolicy:    taskpolicy.TaskPolicy{Verification: taskpolicy.VerifyTargeted},
		turnPolicySet: true,
	}
	if got := a.finalReadinessCheckFor(); !strings.Contains(got.reason, "verification command") {
		t.Fatalf("readiness = %+v, want post-mutation verification", got)
	}
	a.evidence.Record(check)
	if got := a.finalReadinessCheckFor(); got.reason != "" {
		t.Fatalf("readiness after verification = %+v, want ready", got)
	}
}
