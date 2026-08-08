package main

import (
	"os"
	"strings"
	"testing"
)

func TestRenderMechanismLedgerSplitsFiredVsQuiet(t *testing.T) {
	fired := result{task: task{ID: "a"}}
	fired.Trajectory = &trajectorySummary{
		HandoffNudges:  2,
		RoundOutcomeMs: map[string]int64{"handoff_retry": 7_000},
		StreamRetries:  1,
		RecoveryGapMsByKind: map[string]int64{
			"stream_retry": 9_000,
		},
	}
	quiet := result{task: task{ID: "b"}, Passed: true}
	quiet.Trajectory = &trajectorySummary{}

	got := renderMechanismLedger([]result{fired, quiet})
	for _, want := range []string{
		"**Mechanism ledger**",
		"causal rescue rates need an `-ablate` A/B",
		"| handoff_nudge | 2 | 1/2 | 7.0s | 0% | 100% |",
		"| stream_retry | 1 | 1/2 | 9.0s | 0% | 100% |",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ledger missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "| planner |") {
		t.Fatalf("mechanisms that never fired must not render rows:\n%s", got)
	}

	allQuiet := renderMechanismLedger([]result{quiet})
	if !strings.Contains(allQuiet, "all quiet — no extra-round machinery fired across 1 recorded runs") {
		t.Fatalf("all-quiet suite must state the absence:\n%s", allQuiet)
	}
}

func TestSummarizeTrajectorySplitsRecoveryGapByKind(t *testing.T) {
	path := t.TempDir() + "/kinds.trajectory.jsonl"
	lines := []string{
		`{"seq":1,"ts":1000,"event":{"kind":"turn_started"}}`,
		`{"seq":2,"ts":2000,"event":{"kind":"retrying","retryScope":"stream"}}`,
		`{"seq":3,"ts":6000,"event":{"kind":"tool_dispatch","tool":{"id":"a","name":"bash","args":"{}"}}}`,
		`{"seq":4,"ts":6100,"event":{"kind":"tool_result","tool":{"id":"a","name":"bash","durationMs":100,"startedAt":6000,"endedAt":6100}}}`,
		`{"seq":5,"ts":7000,"protocol_recovery":"missing_reasoning_retry_attempted"}`,
		`{"seq":6,"ts":9000,"event":{"kind":"tool_dispatch","tool":{"id":"b","name":"bash","args":"{\"x\":1}"}}}`,
		`{"seq":7,"ts":9100,"event":{"kind":"tool_result","tool":{"id":"b","name":"bash","durationMs":100,"startedAt":9000,"endedAt":9100}}}`,
		`{"seq":8,"ts":10000,"event":{"kind":"turn_done"}}`,
	}
	if err := writeLines(path, lines); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s, err := summarizeTrajectory(path)
	if err != nil {
		t.Fatalf("summarizeTrajectory: %v", err)
	}
	if s.RecoveryGapMsByKind["stream_retry"] != 5000 {
		t.Errorf("stream_retry gap = %d, want 5000", s.RecoveryGapMsByKind["stream_retry"])
	}
	if s.RecoveryGapMsByKind["reasoning_replay"] != 2900 {
		t.Errorf("reasoning_replay gap = %d, want 2900", s.RecoveryGapMsByKind["reasoning_replay"])
	}
	if s.RecoveryRounds != 2 {
		t.Errorf("recovery rounds = %d, want 2", s.RecoveryRounds)
	}
}

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}
