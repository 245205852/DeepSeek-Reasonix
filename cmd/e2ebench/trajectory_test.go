package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummarizeTrajectoryAttributesTimeAndSkipsTruncatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.trajectory.jsonl")
	lines := []string{
		`{"schema_version":1,"seq":1,"ts":1000,"event":{"kind":"turn_started"}}`,
		`{"schema_version":1,"seq":2,"ts":1500,"event":{"kind":"tool_dispatch","tool":{"name":"bash"}}}`,
		`{"schema_version":1,"seq":3,"ts":2000,"event":{"kind":"tool_result","tool":{"name":"bash","durationMs":400}}}`,
		`{"schema_version":1,"seq":4,"ts":2500,"event":{"kind":"tool_result","tool":{"name":"grep","durationMs":300,"parentId":"task-1"}}}`,
		`{"schema_version":1,"seq":5,"ts":4000,"event":{"kind":"turn_done"}}`,
		`{"schema_version":1,"seq":6,"ts":4100,"event":{"kind":"tool_res`, // killed mid-write
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	s, err := summarizeTrajectory(path)
	if err != nil {
		t.Fatalf("summarizeTrajectory: %v", err)
	}
	if s.Records != 5 {
		t.Errorf("records = %d, want 5 (truncated tail skipped)", s.Records)
	}
	if s.SpanMs != 3000 {
		t.Errorf("span = %d, want 3000", s.SpanMs)
	}
	if s.ToolMs != 400 {
		t.Errorf("tool ms = %d, want 400 (subagent call must not double-book)", s.ToolMs)
	}
	if s.ModelMs != 2600 {
		t.Errorf("model ms = %d, want 2600", s.ModelMs)
	}
}

func TestSummarizeTrajectoryDecomposesModelRounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rounds.trajectory.jsonl")
	lines := []string{
		`{"seq":1,"ts":1000,"event":{"kind":"turn_started"}}`,
		`{"seq":2,"ts":3000,"event":{"kind":"tool_dispatch","tool":{"name":"read_file","partial":true}}}`, // round 1 gap: 2000
		`{"seq":3,"ts":3400,"event":{"kind":"tool_dispatch","tool":{"name":"read_file"}}}`,                // same batch: no new round
		`{"seq":4,"ts":5000,"event":{"kind":"tool_result","tool":{"name":"read_file","durationMs":400}}}`,
		`{"seq":5,"ts":5100,"event":{"kind":"tool_dispatch","tool":{"name":"grep","parentId":"task-1"}}}`, // subagent: ignored
		`{"seq":6,"ts":5200,"event":{"kind":"tool_result","tool":{"name":"grep","durationMs":50,"parentId":"task-1"}}}`,
		`{"seq":7,"ts":6000,"event":{"kind":"retrying"}}`,
		`{"seq":8,"ts":9000,"event":{"kind":"tool_dispatch","tool":{"name":"bash"}}}`, // round 2 gap: 9000-5000=4000
		`{"seq":9,"ts":9500,"event":{"kind":"tool_result","tool":{"name":"bash","durationMs":450}}}`,
		`{"seq":10,"ts":12000,"event":{"kind":"turn_done"}}`, // final answer round gap: 2500
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s, err := summarizeTrajectory(path)
	if err != nil {
		t.Fatalf("summarizeTrajectory: %v", err)
	}
	if s.ModelRounds != 3 {
		t.Errorf("model rounds = %d, want 3 (two tool rounds + final answer)", s.ModelRounds)
	}
	if s.ModelGapTotalMs != 8500 {
		t.Errorf("model gap total = %d, want 8500", s.ModelGapTotalMs)
	}
	if s.ModelGapP95Ms != 4000 {
		t.Errorf("model gap p95 = %d, want 4000", s.ModelGapP95Ms)
	}
	if s.Retries != 1 {
		t.Errorf("retries = %d, want 1", s.Retries)
	}
	if s.ToolMs != 850 {
		t.Errorf("tool ms = %d, want 850 (subagent excluded)", s.ToolMs)
	}
}

func TestSummarizeTrajectoryDecomposesBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batches.trajectory.jsonl")
	lines := []string{
		`{"seq":1,"ts":1000,"event":{"kind":"turn_started"}}`,
		// Batch 1: three reads dispatched together, executed overlapping.
		`{"seq":2,"ts":2000,"event":{"kind":"tool_dispatch","tool":{"id":"a","name":"read_file","readOnly":true}}}`,
		`{"seq":3,"ts":2010,"event":{"kind":"tool_dispatch","tool":{"id":"b","name":"read_file","readOnly":true}}}`,
		`{"seq":4,"ts":2020,"event":{"kind":"tool_dispatch","tool":{"id":"c","name":"read_file","readOnly":true}}}`,
		`{"seq":5,"ts":2500,"event":{"kind":"tool_result","tool":{"id":"a","name":"read_file","readOnly":true,"durationMs":400,"startedAt":2050,"endedAt":2450}}}`,
		`{"seq":6,"ts":2500,"event":{"kind":"tool_result","tool":{"id":"b","name":"read_file","readOnly":true,"durationMs":300,"startedAt":2060,"endedAt":2360}}}`,
		`{"seq":7,"ts":2510,"event":{"kind":"tool_result","tool":{"id":"c","name":"read_file","readOnly":true,"durationMs":200,"startedAt":2070,"endedAt":2270}}}`,
		// Batches 2+3: the serialized single-read anti-pattern, streak of two.
		`{"seq":8,"ts":4000,"event":{"kind":"tool_dispatch","tool":{"id":"d","name":"read_file","readOnly":true}}}`,
		`{"seq":9,"ts":4300,"event":{"kind":"tool_result","tool":{"id":"d","name":"read_file","readOnly":true,"durationMs":280,"startedAt":4010,"endedAt":4290}}}`,
		`{"seq":10,"ts":5000,"event":{"kind":"tool_dispatch","tool":{"id":"e","name":"grep","readOnly":true}}}`,
		`{"seq":11,"ts":5200,"event":{"kind":"tool_result","tool":{"id":"e","name":"grep","readOnly":true,"durationMs":180,"startedAt":5010,"endedAt":5190}}}`,
		`{"seq":12,"ts":6000,"event":{"kind":"turn_done"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s, err := summarizeTrajectory(path)
	if err != nil {
		t.Fatalf("summarizeTrajectory: %v", err)
	}
	if s.ToolBatches != 3 || s.TopLevelCalls != 5 || s.MaxBatchSize != 3 {
		t.Errorf("batches=%d calls=%d max=%d, want 3/5/3", s.ToolBatches, s.TopLevelCalls, s.MaxBatchSize)
	}
	if s.ParallelBatches != 1 || s.ParallelSavedMs != 500 {
		t.Errorf("parallel=%d saved=%d, want 1/500 (900 serial vs 400 wall)", s.ParallelBatches, s.ParallelSavedMs)
	}
	if s.SingleReadRounds != 2 || s.SingleReadStreak != 2 {
		t.Errorf("singleReads=%d streak=%d, want 2/2", s.SingleReadRounds, s.SingleReadStreak)
	}
	if s.ToolWallMs != 860 {
		t.Errorf("tool wall = %d, want 860 (400 union + 280 + 180)", s.ToolWallMs)
	}
	if s.ToolMs != 1360 {
		t.Errorf("tool ms = %d, want 1360 (duration sum)", s.ToolMs)
	}
	if s.ModelMs != 4140 {
		t.Errorf("model ms = %d, want 4140 (span 5000 − wall 860)", s.ModelMs)
	}
	if s.ModelRounds != 4 {
		t.Errorf("model rounds = %d, want 4 (three tool rounds + final answer)", s.ModelRounds)
	}
	if s.StartDelayP95Ms != 50 {
		t.Errorf("start delay p95 = %d, want 50", s.StartDelayP95Ms)
	}
}

func TestSummarizeTrajectoryAnchorsStartDelayToFullDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.trajectory.jsonl")
	lines := []string{
		`{"seq":1,"ts":1000,"event":{"kind":"turn_started"}}`,
		// Streamed partial announcement ~900ms before the full dispatch; the
		// stream tail must not be booked as pre-exec queueing.
		`{"seq":2,"ts":2000,"event":{"kind":"tool_dispatch","tool":{"id":"a","name":"bash","partial":true}}}`,
		`{"seq":3,"ts":2900,"event":{"kind":"tool_dispatch","tool":{"id":"a","name":"bash"}}}`,
		`{"seq":4,"ts":3000,"event":{"kind":"tool_result","tool":{"id":"a","name":"bash","durationMs":90,"startedAt":2910,"endedAt":3000}}}`,
		`{"seq":5,"ts":4000,"event":{"kind":"turn_done"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s, err := summarizeTrajectory(path)
	if err != nil {
		t.Fatalf("summarizeTrajectory: %v", err)
	}
	if s.TopLevelCalls != 1 || s.ToolBatches != 1 {
		t.Errorf("calls=%d batches=%d, want 1/1 (partial+full is one call)", s.TopLevelCalls, s.ToolBatches)
	}
	if s.StartDelayP95Ms != 10 {
		t.Errorf("start delay p95 = %d, want 10 (2910 − full dispatch 2900)", s.StartDelayP95Ms)
	}
}

func TestSummarizeTrajectorySplitsCleanAndRecoveryRounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.trajectory.jsonl")
	lines := []string{
		`{"seq":1,"ts":1000,"event":{"kind":"turn_started"}}`,
		// Round 1 (clean, gap 1000).
		`{"seq":2,"ts":2000,"event":{"kind":"tool_dispatch","tool":{"id":"a","name":"bash"}}}`,
		`{"seq":3,"ts":2100,"event":{"kind":"tool_result","tool":{"id":"a","name":"bash","durationMs":100,"startedAt":2000,"endedAt":2100}}}`,
		// Round 2 (gap 8900): stream-interrupted retry mid-gap.
		`{"seq":4,"ts":4000,"event":{"kind":"retrying","retryAttempt":1,"retryScope":"stream"}}`,
		`{"seq":5,"ts":11000,"event":{"kind":"tool_dispatch","tool":{"id":"b","name":"bash"}}}`,
		`{"seq":6,"ts":11100,"event":{"kind":"tool_result","tool":{"id":"b","name":"bash","durationMs":100,"startedAt":11000,"endedAt":11100}}}`,
		// Round 3 (gap 2900): missing-reasoning exact replay.
		`{"seq":7,"ts":12000,"protocol_recovery":"missing_reasoning_retry_attempted"}`,
		`{"seq":8,"ts":14000,"event":{"kind":"tool_dispatch","tool":{"id":"c","name":"bash"}}}`,
		`{"seq":9,"ts":14100,"event":{"kind":"tool_result","tool":{"id":"c","name":"bash","durationMs":100,"startedAt":14000,"endedAt":14100}}}`,
		// Final answer round (gap 5900): empty-final retry.
		`{"seq":10,"ts":16000,"event":{"kind":"notice","code":"empty_final"}}`,
		`{"seq":11,"ts":20000,"event":{"kind":"turn_done"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s, err := summarizeTrajectory(path)
	if err != nil {
		t.Fatalf("summarizeTrajectory: %v", err)
	}
	if s.StreamRetries != 1 || s.HeaderRetries != 0 || s.Retries != 1 {
		t.Errorf("stream=%d header=%d retries=%d, want 1/0/1", s.StreamRetries, s.HeaderRetries, s.Retries)
	}
	if s.ReasoningReplays != 1 || s.EmptyFinalRetries != 1 {
		t.Errorf("replays=%d emptyFinal=%d, want 1/1", s.ReasoningReplays, s.EmptyFinalRetries)
	}
	if s.ModelRounds != 4 || s.RecoveryRounds != 3 {
		t.Errorf("rounds=%d recovery=%d, want 4/3", s.ModelRounds, s.RecoveryRounds)
	}
	if s.RecoveryGapMs != 8900+2900+5900 {
		t.Errorf("recovery gap = %d, want 17700", s.RecoveryGapMs)
	}
	if s.CleanGapP95Ms != 1000 {
		t.Errorf("clean gap p95 = %d, want 1000 (only round 1 is clean)", s.CleanGapP95Ms)
	}
	if s.ModelGapP95Ms != 8900 {
		t.Errorf("recovery-inclusive gap p95 = %d, want 8900", s.ModelGapP95Ms)
	}
}

func TestRenderTimeAttributionIncludesBatchingLine(t *testing.T) {
	r := result{task: task{ID: "a"}, Passed: true}
	r.Trajectory = &trajectorySummary{
		Records: 12, SpanMs: 5000, ToolMs: 1360, ToolWallMs: 860, ModelMs: 4140,
		ModelRounds: 4, ModelGapTotalMs: 3990,
		ToolBatches: 3, TopLevelCalls: 5, MaxBatchSize: 3,
		ParallelBatches: 1, ParallelSavedMs: 500,
		SingleReadRounds: 2, SingleReadStreak: 2, StartDelayP95Ms: 50,
	}
	got := renderTimeAttribution([]result{r})
	for _, want := range []string{
		"**Batching** (3 tool rounds)",
		"**calls/round** 1.7",
		"**single-read rounds** 2 (67%)",
		"**parallel rounds** 1 (saved 0.5s)",
		"**start-delay p95** 50ms",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("batching line missing %q:\n%s", want, got)
		}
	}
}

func TestRenderBodyReportsPerSolvedEfficiency(t *testing.T) {
	solved := result{task: task{ID: "a"}, Passed: true, WallMs: 60_000}
	solved.Steps = 6
	solved.ToolCalls = 9
	solved.Trajectory = &trajectorySummary{ModelRounds: 5}
	failed := result{task: task{ID: "b"}, WallMs: 40_000}
	failed.Steps = 8
	failed.ToolCalls = 3
	failed.Trajectory = &trajectorySummary{ModelRounds: 7}

	got := renderBody([]result{solved, failed})
	for _, want := range []string{
		"**Per solved task:**",
		"**model requests** 14.0", // failures' spend charged to the solve
		"tool calls 12.0",
		"wall 1m40s",
		"model rounds 12.0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("per-solved line missing %q:\n%s", want, got)
		}
	}

	if got := renderBody([]result{failed}); strings.Contains(got, "Per solved task") {
		t.Fatalf("no solves must mean no per-solved line:\n%s", got)
	}
}

func TestRenderBodyIncludesTimeAttributionOnlyForRecordedRuns(t *testing.T) {
	plain := result{task: task{ID: "a"}, Passed: true}
	if got := renderBody([]result{plain}); strings.Contains(got, "Time attribution") {
		t.Fatalf("unrecorded run must not report time attribution:\n%s", got)
	}

	recorded := plain
	recorded.Trajectory = &trajectorySummary{Records: 5, SpanMs: 3000, ToolMs: 1000, ModelMs: 2000}
	got := renderBody([]result{recorded})
	if !strings.Contains(got, "Time attribution") || !strings.Contains(got, "(1 recorded runs)") {
		t.Fatalf("recorded run missing time attribution:\n%s", got)
	}
}
