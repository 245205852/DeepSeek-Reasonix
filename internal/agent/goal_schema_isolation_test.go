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

type plannerPhaseOnlyTool struct{}

func (plannerPhaseOnlyTool) Name() string        { return "planner_phase_only" }
func (plannerPhaseOnlyTool) Description() string { return "planner phase-only test tool" }
func (plannerPhaseOnlyTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (plannerPhaseOnlyTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "phase-only", nil
}
func (plannerPhaseOnlyTool) ReadOnly() bool     { return true }
func (plannerPhaseOnlyTool) PlanModeSafe() bool { return false }

func TestPlannerToolRegistryExcludesNonContextualPlanUnsafeTools(t *testing.T) {
	parent := tool.NewRegistry()
	parent.Add(plannerPhaseOnlyTool{})
	if _, ok := PlannerToolRegistry(parent).Get("planner_phase_only"); ok {
		t.Fatal("two-model Planner exposed a PlanModeSafe=false custom tool")
	}
}

func TestGoalContextChangesOnlyUpdateGoalVisibility(t *testing.T) {
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
	if string(ordinarySchemas) == string(goalSchemas) {
		t.Fatalf("Goal context did not expose update_goal:\nordinary=%s\ngoal=%s", ordinarySchemas, goalSchemas)
	}
	if slices.Contains(toolSchemaNames(ordinary.requests[0].Tools), "update_goal") {
		t.Fatalf("ordinary request exposed update_goal: %s", ordinarySchemas)
	}
	if !slices.Contains(toolSchemaNames(goal.requests[0].Tools), "update_goal") {
		t.Fatalf("Goal request hid update_goal: %s", goalSchemas)
	}
}

func TestContextualToolSchemasStayStableWithinEachGoalPhase(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	reg := tool.NewRegistry()
	reg.Add(goalTool)
	reg.Add(fakeTool{name: "read_file", readOnly: true})

	marshal := func(ctx context.Context) string {
		t.Helper()
		raw, err := json.Marshal(reg.SchemasForContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	ordinaryCtx := context.Background()
	goalCtx := tool.WithGoalTurnRecorder(context.Background(), requestGoalRecorder{})
	ordinary := marshal(ordinaryCtx)
	goal := marshal(goalCtx)
	if ordinary != marshal(ordinaryCtx) {
		t.Fatal("ordinary-phase schema bytes changed between identical requests")
	}
	if goal != marshal(goalCtx) {
		t.Fatal("Goal-phase schema bytes changed between identical requests")
	}
	if ordinary == goal {
		t.Fatal("Goal phase transition did not produce the expected one-time schema difference")
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

func TestMixedContextUnavailableBatchExecutesValidToolsAndRepairsOnce(t *testing.T) {
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
	if got := lastUser(prov.requests[1]); !strings.Contains(got, "update_goal") || !strings.Contains(got, "visible answer text") {
		t.Fatalf("repair instruction = %q", got)
	}
	if slices.Contains(toolSchemaNames(prov.requests[1].Tools), "update_goal") || !slices.Contains(toolSchemaNames(prov.requests[1].Tools), "read_file") {
		t.Fatalf("repair schemas = %v", toolSchemaNames(prov.requests[1].Tools))
	}
	if got := toolResultByID(sess, "goal"); !strings.Contains(got, "only available while an active goal turn") {
		t.Fatalf("unavailable result = %q", got)
	}
	if got := toolResultByID(sess, "read"); got != "read_file done" {
		t.Fatalf("valid result = %q", got)
	}
}

func TestRepeatedMixedContextUnavailableBatchStopsBeforeReexecution(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	var validCalls int32
	reg := tool.NewRegistry()
	reg.Add(goalTool)
	reg.Add(fakeTool{name: "read_file", readOnly: true, calls: &validCalls})
	firstMixed := []provider.Chunk{
		toolCallChunk("goal", "update_goal", `{"status":"complete"}`),
		toolCallChunk("read", "read_file", `{}`),
		{Type: provider.ChunkDone},
	}
	secondMixed := []provider.Chunk{
		toolCallChunk("goal-2", "update_goal", `{"status":"complete"}`),
		toolCallChunk("read-2", "read_file", `{}`),
		{Type: provider.ChunkDone},
	}
	prov := &scriptedProvider{name: "repeated-mixed", turns: [][]provider.Chunk{firstMixed, secondMixed}}
	sess := NewSession("sys")
	a := New(prov, reg, sess, Options{MaxSteps: 1}, event.Discard)

	err := a.Run(context.Background(), "inspect and answer")
	if err == nil || !strings.Contains(err.Error(), "repeatedly called context-unavailable tools") {
		t.Fatalf("Run error = %v, want repeated contextual misuse", err)
	}
	if got := atomic.LoadInt32(&validCalls); got != 1 {
		t.Fatalf("valid tool calls = %d, want second mixed batch blocked before execution", got)
	}
	if got := toolResultByID(sess, "read-2"); !strings.Contains(got, "called again after the repair instruction") {
		t.Fatalf("second batch pairing result = %q", got)
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
		if slices.Contains(toolSchemaNames(req.Tools), "update_goal") {
			t.Fatalf("child provider request %d exposed update_goal: %v", i+1, toolSchemaNames(req.Tools))
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
		if slices.Contains(toolSchemaNames(req.Tools), "update_goal") {
			t.Fatalf("planner request %d exposed update_goal: %v", i+1, toolSchemaNames(req.Tools))
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
	ctx := tool.WithGoalTurnRecorder(context.Background(), requestGoalRecorder{})
	run, err := task.prepareTranscriptRunWithPrompt(ctx, reg, "model", "medium", "parent-session", "call-1", "", "", "child system", "task", "inspect")
	if err != nil {
		t.Fatalf("prepareTranscriptRunWithPrompt: %v", err)
	}
	defer run.Release()
	if slices.Contains(run.Meta.ToolScope, "update_goal") || !slices.Contains(run.Meta.ToolScope, "read_file") {
		t.Fatalf("subagent tool scope = %v, want only child-visible tools", run.Meta.ToolScope)
	}
	_, wantHash := toolIdentity(reg, reg.SchemasForContext(subagentProviderContext(ctx)))
	if run.Meta.ToolSchemaHash != wantHash {
		t.Fatalf("subagent schema hash = %q, want %q", run.Meta.ToolSchemaHash, wantHash)
	}
	_, staticHash := toolIdentity(reg, reg.Schemas())
	if run.Meta.ToolSchemaHash == staticHash {
		t.Fatal("subagent identity used static schemas and included parent-only update_goal")
	}
}
