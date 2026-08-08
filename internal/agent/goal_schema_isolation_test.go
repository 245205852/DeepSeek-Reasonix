package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

type requestGoalRecorder struct{}

func (requestGoalRecorder) RecordGoalReport(report tool.GoalReport) (string, error) {
	return "recorded " + report.Status, nil
}

type childIsolationGoalRecorder struct {
	reports []tool.GoalReport
}

func (r *childIsolationGoalRecorder) RecordGoalReport(report tool.GoalReport) (string, error) {
	r.reports = append(r.reports, report)
	return "recorded " + report.Status, nil
}

func TestGoalContextKeepsProviderSchemasStable(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	reg := tool.NewRegistry()
	reg.Add(goalTool)
	ordinary := &scriptedProvider{name: "ordinary", turns: [][]provider.Chunk{
		{{Type: provider.ChunkText, Text: "ordinary"}, {Type: provider.ChunkDone}},
	}}
	ordinaryAgent := New(ordinary, reg, NewSession("sys"), Options{}, event.Discard)
	if err := ordinaryAgent.Run(context.Background(), "answer normally"); err != nil {
		t.Fatalf("ordinary Run: %v", err)
	}
	goal := &scriptedProvider{name: "goal", turns: [][]provider.Chunk{
		{{Type: provider.ChunkText, Text: "goal"}, {Type: provider.ChunkDone}},
	}}
	goalAgent := New(goal, reg, NewSession("sys"), Options{}, event.Discard)
	ctx := tool.WithGoalTurnRecorder(context.Background(), requestGoalRecorder{})
	if err := goalAgent.Run(ctx, "continue goal"); err != nil {
		t.Fatalf("Goal Run: %v", err)
	}
	ordinarySchemas, err := json.Marshal(ordinary.requests[0].Tools)
	if err != nil {
		t.Fatal(err)
	}
	goalSchemas, err := json.Marshal(goal.requests[0].Tools)
	if err != nil {
		t.Fatal(err)
	}
	if string(ordinarySchemas) != string(goalSchemas) {
		t.Fatalf("Goal context changed provider schemas:\nordinary=%s\ngoal=%s", ordinarySchemas, goalSchemas)
	}
	if !slices.Contains(toolSchemaNames(ordinary.requests[0].Tools), "update_goal") || !slices.Contains(toolSchemaNames(goal.requests[0].Tools), "update_goal") {
		t.Fatalf("stable requests lost update_goal: ordinary=%s goal=%s", ordinarySchemas, goalSchemas)
	}
}

func TestGoalRequestExposesUpdateGoal(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	reg := tool.NewRegistry()
	reg.Add(goalTool)
	prov := &scriptedProvider{name: "p", turns: [][]provider.Chunk{
		{{Type: provider.ChunkText, Text: "Goal work continues."}, {Type: provider.ChunkDone}},
	}}
	a := New(prov, reg, NewSession("sys"), Options{}, event.Discard)
	ctx := tool.WithGoalTurnRecorder(context.Background(), requestGoalRecorder{})
	if err := a.Run(ctx, "continue goal"); err != nil {
		t.Fatalf("Goal answer: %v", err)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(prov.requests))
	}
	if !slices.Contains(toolSchemaNames(prov.requests[0].Tools), "update_goal") {
		t.Fatal("Goal provider request did not expose update_goal")
	}
}

func TestMixedOutOfContextGoalBatchExecutesValidToolsWithStableSchemas(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	var validCalls int32
	reg := tool.NewRegistry()
	reg.Add(goalTool)
	reg.Add(fakeTool{name: "read_file", readOnly: true, calls: &validCalls})
	prov := &scriptedProvider{name: "mixed", turns: [][]provider.Chunk{
		{
			toolCallChunk("goal", "update_goal", `{"status":"complete"}`),
			toolCallChunk("read", "read_file", `{}`),
			{Type: provider.ChunkDone},
		},
		{{Type: provider.ChunkText, Text: "Visible answer after collecting the valid result."}, {Type: provider.ChunkDone}},
	}}
	sess := NewSession("sys")
	a := New(prov, reg, sess, Options{}, event.Discard)

	if err := a.Run(context.Background(), "inspect and answer"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&validCalls); got != 1 {
		t.Fatalf("valid tool calls = %d, want 1", got)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want one repair", len(prov.requests))
	}
	if got := lastUser(prov.requests[1]); got != "inspect and answer" {
		t.Fatalf("stable request unexpectedly added a schema repair instruction = %q", got)
	}
	if !slices.Contains(toolSchemaNames(prov.requests[1].Tools), "update_goal") || !slices.Contains(toolSchemaNames(prov.requests[1].Tools), "read_file") {
		t.Fatalf("stable schemas = %v", toolSchemaNames(prov.requests[1].Tools))
	}
	if got := toolResultByID(sess, "goal"); !strings.Contains(got, "only available while an active goal turn") {
		t.Fatalf("unavailable result = %q", got)
	}
	if got := toolResultByID(sess, "read"); got != "read_file done" {
		t.Fatalf("valid result = %q", got)
	}
}

func TestSubAgentDoesNotInheritParentGoalRecorder(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	reg := tool.NewRegistry()
	reg.Add(goalTool)
	prov := &scriptedProvider{name: "goal-child", turns: [][]provider.Chunk{
		{toolCallChunk("goal", "update_goal", `{"status":"complete"}`), {Type: provider.ChunkDone}},
		{{Type: provider.ChunkText, Text: "Child result."}, {Type: provider.ChunkDone}},
	}}
	recorder := &childIsolationGoalRecorder{}
	ctx := tool.WithGoalTurnRecorder(context.Background(), recorder)
	sess := NewSession("child system")

	answer, err := RunSubAgentWithSession(ctx, prov, reg, sess, "inspect the task", Options{}, event.Discard)
	if err != nil {
		t.Fatalf("Goal child: %v", err)
	}
	if answer != "Child result." {
		t.Fatalf("Goal child answer = %q", answer)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want hallucinated call plus repair", len(prov.requests))
	}
	for i, req := range prov.requests {
		if !slices.Contains(toolSchemaNames(req.Tools), "update_goal") {
			t.Fatalf("child provider request %d lost stable update_goal schema: %v", i+1, toolSchemaNames(req.Tools))
		}
	}
	if len(recorder.reports) != 0 {
		t.Fatalf("child wrote reports into parent Goal recorder: %+v", recorder.reports)
	}
	if got := lastToolResult(sess, "update_goal"); !strings.Contains(got, "only available while an active goal turn") {
		t.Fatalf("child update_goal result = %q", got)
	}
}

type coordinatorGoalRecorder struct {
	reports []tool.GoalReport
}

func (r *coordinatorGoalRecorder) RecordGoalReport(report tool.GoalReport) (string, error) {
	r.reports = append(r.reports, report)
	return "recorded " + report.Status, nil
}

func TestCoordinatorPlannerCannotReportExecutorGoalDisposition(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	reg := tool.NewRegistry()
	reg.Add(goalTool)
	planner := &mockProvider{name: "planner", streams: [][]provider.Chunk{
		{toolCallChunk("planner-goal", "update_goal", `{"status":"complete"}`), {Type: provider.ChunkDone}},
		{{Type: provider.ChunkText, Text: "1. inspect the implementation\n2. apply and verify the fix"}, {Type: provider.ChunkDone}},
	}}
	exec := &mockProvider{name: "executor", chunks: []provider.Chunk{
		{Type: provider.ChunkText, Text: "Implemented and verified."},
		{Type: provider.ChunkDone},
	}}
	plannerSess := NewSession("planner-sys")
	executor := New(exec, reg, NewSession("exec-sys"), Options{}, event.Discard)
	customPlannerReg := tool.NewRegistry()
	customPlannerReg.Add(goalTool)
	coord := NewCoordinator(planner, plannerSess, nil, customPlannerReg, Options{}, executor, 0, event.Discard, nil)
	recorder := &coordinatorGoalRecorder{}
	ctx := tool.WithGoalTurnRecorder(context.Background(), recorder)

	if err := coord.Run(ctx, "fix the goal bug"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(planner.requests) != 2 {
		t.Fatalf("planner requests = %d, want hallucinated call plus repair", len(planner.requests))
	}
	for i, req := range planner.requests {
		if !slices.Contains(toolSchemaNames(req.Tools), "update_goal") {
			t.Fatalf("planner request %d lost stable update_goal schema: %v", i+1, toolSchemaNames(req.Tools))
		}
	}
	if got := lastToolResult(plannerSess, "update_goal"); !strings.Contains(got, "only available while an active goal turn") {
		t.Fatalf("planner update_goal result = %q", got)
	}
	if len(exec.requests) == 0 {
		t.Fatal("executor made no requests")
	}
	for i, req := range exec.requests {
		if !slices.Contains(toolSchemaNames(req.Tools), "update_goal") {
			t.Fatalf("executor request %d lost update_goal after planner isolation: %v", i+1, toolSchemaNames(req.Tools))
		}
	}
	if len(recorder.reports) != 0 {
		t.Fatalf("planner wrote reports into executor Goal recorder: %+v", recorder.reports)
	}
}

func TestSubagentIdentityUsesEffectiveChildToolSchemas(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	reg := tool.NewRegistry()
	reg.Add(goalTool)
	reg.Add(fakeTool{name: "read_file", readOnly: true})
	store := NewSubagentStore(t.TempDir())
	task := &TaskTool{transcripts: store, sysPrompt: "child system", workspaceRoot: t.TempDir()}
	run, err := task.prepareTranscriptRunWithPrompt(reg, "model", "medium", "parent-session", "call-1", "", "", "child system", "task", "inspect")
	if err != nil {
		t.Fatalf("prepareTranscriptRunWithPrompt: %v", err)
	}
	defer run.Release()
	if !slices.Contains(run.Meta.ToolScope, "update_goal") || !slices.Contains(run.Meta.ToolScope, "read_file") {
		t.Fatalf("subagent tool scope = %v, want stable registry schemas", run.Meta.ToolScope)
	}
	_, wantHash := toolIdentity(reg)
	if run.Meta.ToolSchemaHash != wantHash {
		t.Fatalf("subagent schema hash = %q, want %q", run.Meta.ToolSchemaHash, wantHash)
	}
}
