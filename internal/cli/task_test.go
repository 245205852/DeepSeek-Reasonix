package cli

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"reasonix/internal/taskmonitor"
)

// testStore builds an InMemoryStore with a few preloaded tasks and events.
func testStore(t *testing.T) *taskmonitor.InMemoryStore {
	t.Helper()
	s := taskmonitor.NewInMemoryStore()

	seed := func(i int) time.Time { return time.Date(2025, 1, 1, 0, 0, i, 0, time.UTC) }

	// Project A: two tasks
	mustUpsert(t, s, "/proj-a", taskmonitor.TaskSnapshot{
		SchemaVersion: 1, TaskID: "a1", SessionID: "s1",
		State: taskmonitor.TaskStateRunning, CreatedAt: seed(1), UpdatedAt: seed(10),
	})
	mustUpsert(t, s, "/proj-a", taskmonitor.TaskSnapshot{
		SchemaVersion: 1, TaskID: "a2", SessionID: "s2",
		State: taskmonitor.TaskStateSucceeded, CreatedAt: seed(2), UpdatedAt: seed(11),
	})

	// Events for a1
	for i := 1; i <= 3; i++ {
		mustAppend(t, s, "/proj-a", taskmonitor.TaskEvent{
			Sequence: i, Timestamp: seed(i), EventType: "state_change",
			TaskID: "a1", SessionID: "s1", State: taskmonitor.TaskStateRunning,
		})
	}

	// Project B: one task
	mustUpsert(t, s, "/proj-b", taskmonitor.TaskSnapshot{
		SchemaVersion: 1, TaskID: "b1", SessionID: "s3",
		State: taskmonitor.TaskStateFailed, CreatedAt: seed(3), UpdatedAt: seed(12),
		ErrorCode: "EXIT_1",
	})
	return s
}

func mustUpsert(t *testing.T, s *taskmonitor.InMemoryStore, proj string, snap taskmonitor.TaskSnapshot) {
	t.Helper()
	if err := s.UpsertTask(proj, snap); err != nil {
		t.Fatal(err)
	}
}

func mustAppend(t *testing.T, s *taskmonitor.InMemoryStore, proj string, ev taskmonitor.TaskEvent) {
	t.Helper()
	if err := s.AppendEvent(proj, ev); err != nil {
		t.Fatal(err)
	}
}

// captureOut runs fn and returns (exitCode, capturedStdout).
func captureOut(fn func() int) (int, string) {
	orig := taskStore
	defer func() { taskStore = orig }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	ec := fn()
	w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return ec, string(data)
}

// --- JSON schema tests ---

func TestTaskList_JSON_SchemaVersion(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskListCmd(s, []string{"--json"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.SchemaVersion != 1 {
		t.Errorf("schema_version=%d, want 1", v.SchemaVersion)
	}
}

func TestTaskList_JSON_Empty(t *testing.T) {
	s := taskmonitor.NewInMemoryStore()
	taskStore = s

	exit, out := captureOut(func() int {
		return taskListCmd(s, []string{"--json", "--dir", "/no-such"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		Tasks []taskmonitor.TaskSnapshot `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(v.Tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(v.Tasks))
	}
}

func TestTaskList_JSON_FieldsPresent(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskListCmd(s, []string{"--json"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		Tasks []taskmonitor.TaskSnapshot `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(v.Tasks) < 1 {
		t.Fatal("expected at least 1 task")
	}
	tsk := v.Tasks[0]
	if tsk.SchemaVersion != 1 || tsk.TaskID == "" || tsk.SessionID == "" ||
		tsk.State == "" || tsk.CreatedAt.IsZero() || tsk.UpdatedAt.IsZero() {
		t.Errorf("missing required fields in %+v", tsk)
	}
}

func TestTaskList_JSON_ProjectIsolation(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskListCmd(s, []string{"--json", "--dir", "/proj-a"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		Tasks []taskmonitor.TaskSnapshot `json:"tasks"`
	}
	json.Unmarshal([]byte(out), &v)
	for _, tsk := range v.Tasks {
		if tsk.TaskID == "b1" {
			t.Error("project-b task leaked into project-a")
		}
	}
}

// --- status ---

func TestTaskStatus_JSON_Found(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskStatusCmd(s, []string{"--json", "a1"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		Task taskmonitor.TaskSnapshot `json:"task"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Task.TaskID != "a1" {
		t.Errorf("expected a1, got %s", v.Task.TaskID)
	}
}

func TestTaskStatus_JSON_NotFound(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskStatusCmd(s, []string{"--json", "ghost"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		Task *taskmonitor.TaskSnapshot `json:"task"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Task != nil {
		t.Errorf("expected null task, got %+v", v.Task)
	}
}

func TestTaskStatus_JSON_SchemaVersion(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskStatusCmd(s, []string{"--json", "a1"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		SchemaVersion int `json:"schema_version"`
	}
	json.Unmarshal([]byte(out), &v)
	if v.SchemaVersion != 1 {
		t.Errorf("schema_version=%d", v.SchemaVersion)
	}
}

// --- events ---

func TestTaskEvents_JSON_SchemaVersion(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskEventsCmd(s, []string{"--json", "a1"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		SchemaVersion int `json:"schema_version"`
	}
	json.Unmarshal([]byte(out), &v)
	if v.SchemaVersion != 1 {
		t.Errorf("schema_version=%d", v.SchemaVersion)
	}
}

func TestTaskEvents_JSON_FieldsPresent(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskEventsCmd(s, []string{"--json", "a1"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		TaskID string                  `json:"task_id"`
		Events []taskmonitor.TaskEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.TaskID != "a1" {
		t.Errorf("task_id=%q", v.TaskID)
	}
	if len(v.Events) != 3 {
		t.Errorf("expected 3 events, got %d", len(v.Events))
	}
	for _, ev := range v.Events {
		if ev.Sequence <= 0 || ev.TaskID == "" || ev.EventType == "" || ev.State == "" || ev.Timestamp.IsZero() {
			t.Errorf("missing required fields in event %+v", ev)
		}
	}
}

func TestTaskEvents_JSON_AfterCursor(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskEventsCmd(s, []string{"--json", "--after", "1", "a1"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		Events []taskmonitor.TaskEvent `json:"events"`
	}
	json.Unmarshal([]byte(out), &v)
	if len(v.Events) != 2 {
		t.Errorf("after seq 1: expected 2 events, got %d", len(v.Events))
	}
	if v.Events[0].Sequence != 2 || v.Events[1].Sequence != 3 {
		t.Errorf("unexpected sequences: %d, %d", v.Events[0].Sequence, v.Events[1].Sequence)
	}
}

func TestTaskEvents_JSONL_Format(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskEventsCmd(s, []string{"--jsonl", "a1"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 JSONL lines, got %d", len(lines))
	}
	for _, line := range lines {
		var ev taskmonitor.TaskEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("invalid JSONL line: %v", err)
		}
	}
}

func TestTaskEvents_JSON_NoSensitiveFields(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskEventsCmd(s, []string{"--json", "a1"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	for _, forbidden := range []string{"prompt", "tool_args", "tool_result", "reasoning"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output contains forbidden field %q", forbidden)
		}
	}
}

func TestTaskEvents_JSON_EmptyForUnknownTask(t *testing.T) {
	s := testStore(t)
	taskStore = s

	exit, out := captureOut(func() int {
		return taskEventsCmd(s, []string{"--json", "ghost"})
	})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var v struct {
		Events []taskmonitor.TaskEvent `json:"events"`
	}
	json.Unmarshal([]byte(out), &v)
	if len(v.Events) != 0 {
		t.Errorf("expected empty, got %d events", len(v.Events))
	}
}

func TestTaskList_NoFlagErrors(t *testing.T) {
	s := taskmonitor.NewInMemoryStore()
	exit, _ := captureOut(func() int {
		return taskListCmd(s, []string{})
	})
	if exit == 0 {
		t.Error("expected non-zero exit without --json")
	}
}

func TestTaskStatus_MissingID(t *testing.T) {
	s := taskmonitor.NewInMemoryStore()
	exit, _ := captureOut(func() int {
		return taskStatusCmd(s, []string{"--json"})
	})
	if exit == 0 {
		t.Error("expected non-zero exit without ID")
	}
}

func TestTaskEvents_NoFlag(t *testing.T) {
	s := taskmonitor.NewInMemoryStore()
	exit, _ := captureOut(func() int {
		return taskEventsCmd(s, []string{"a1"})
	})
	if exit == 0 {
		t.Error("expected non-zero exit without --json/--jsonl")
	}
}

// --- CLI wiring ---

func TestTaskCommand_Dispatch(t *testing.T) {
	s := testStore(t)
	taskStore = s

	// list
	exit, out := captureOut(func() int {
		return taskCommand([]string{"list", "--json"})
	})
	if exit != 0 || !strings.Contains(out, "task_id") {
		t.Errorf("task list failed: exit=%d out=%s", exit, out)
	}

	// status
	exit, out = captureOut(func() int {
		return taskCommand([]string{"status", "--json", "a1"})
	})
	if exit != 0 || !strings.Contains(out, "a1") {
		t.Errorf("task status failed: exit=%d out=%s", exit, out)
	}

	// events
	exit, out = captureOut(func() int {
		return taskCommand([]string{"events", "--json", "a1"})
	})
	if exit != 0 || !strings.Contains(out, "event_type") {
		t.Errorf("task events failed: exit=%d out=%s", exit, out)
	}

	// unknown subcommand
	exit, _ = captureOut(func() int {
		return taskCommand([]string{"unknown"})
	})
	if exit == 0 {
		t.Error("expected non-zero for unknown subcommand")
	}
}

func TestTaskCommand_UnknownSubcommand(t *testing.T) {
	exit, _ := captureOut(func() int {
		return taskCommand([]string{"bogus"})
	})
	if exit != 2 {
		t.Errorf("exit=%d, want 2", exit)
	}
}
