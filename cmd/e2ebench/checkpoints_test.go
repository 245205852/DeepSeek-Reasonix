package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTaskVerify(t *testing.T, taskDir, script string) {
	t.Helper()
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "verify.sh"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGradeCheckpointsFindsEarliestCorrectState(t *testing.T) {
	taskDir := filepath.Join(t.TempDir(), "task")
	writeTaskVerify(t, taskDir, "#!/usr/bin/env bash\ngrep -q done answer.txt\n")

	snaps := t.TempDir()
	mk := func(seq int, elapsed int64, content string) checkpoint {
		dir := filepath.Join(snaps, filepath.Base(strings.ReplaceAll(content, " ", "-"))+"-cp")
		dir = filepath.Join(snaps, filepath.Base(dir)+"-"+strings.ReplaceAll(content, " ", "_"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "answer.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return checkpoint{Seq: seq, ElapsedMs: elapsed, dir: dir}
	}
	checkpoints := gradeCheckpoints([]checkpoint{
		mk(1, 18_000, "not yet"),
		mk(2, 63_000, "done"),
		mk(3, 128_000, "done and polished"),
	}, taskDir)
	if checkpoints[0].Pass || !checkpoints[1].Pass || !checkpoints[2].Pass {
		t.Fatalf("pass flags = %+v", checkpoints)
	}

	firstMs, broke := firstCorrect(checkpoints, true)
	if firstMs != 63_000 || broke {
		t.Fatalf("firstCorrect = %d, %v; want 63000, false", firstMs, broke)
	}

	// A run whose final state failed after a passing snapshot is the
	// solved-then-broke alarm.
	firstMs, broke = firstCorrect(checkpoints, false)
	if firstMs != 63_000 || !broke {
		t.Fatalf("solved-then-broke = %d, %v; want 63000, true", firstMs, broke)
	}

	if ms, broke := firstCorrect([]checkpoint{{Seq: 1, ElapsedMs: 5, dir: snaps}}, true); ms != 0 || broke {
		t.Fatalf("no passing snapshot must yield 0/false, got %d/%v", ms, broke)
	}
}

func TestSnapshotterCapturesWorkspaceChanges(t *testing.T) {
	work := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "code.py"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := startSnapshotter(work, dst, time.Now())

	time.Sleep(2 * snapshotPollInterval)
	// The metrics sidecar updating must not trigger a snapshot on its own.
	if err := os.WriteFile(filepath.Join(work, ".run-metrics.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * snapshotPollInterval)
	if err := os.WriteFile(filepath.Join(work, "code.py"), []byte("v2 changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * snapshotPollInterval)
	taken := snap.halt()

	if len(taken) != 1 {
		t.Fatalf("snapshots = %d (%+v), want exactly 1 (the code change; metrics writes excluded)", len(taken), taken)
	}
	data, err := os.ReadFile(filepath.Join(taken[0].dir, "code.py"))
	if err != nil || string(data) != "v2 changed" {
		t.Fatalf("snapshot content = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(taken[0].dir, ".run-metrics.json")); !os.IsNotExist(err) {
		t.Fatal("metrics sidecar must be stripped from snapshots")
	}
}

func TestKPILineIncludesTTFCS(t *testing.T) {
	r := result{task: task{ID: "a"}, Passed: true, WallMs: 142_000, Attempt: 1, TTCSMs: 142_000}
	r.FirstCorrectMs = 63_000
	r.PostSolveWasteMs = 79_000
	got := renderBody([]result{r})
	for _, want := range []string{
		"**TTFCS median** 1m03s",
		"**post-solve waste median** 1m19s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("KPI line missing %q:\n%s", want, got)
		}
	}
}
