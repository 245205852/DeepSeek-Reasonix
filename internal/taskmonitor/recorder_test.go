package taskmonitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"reasonix/internal/jobs"
)

func newRecorderForTest(t *testing.T, projectDir string) (*TaskRecorder, *FileStore) {
	t.Helper()
	store := NewFileStore(".reasonix/tasks")
	r := NewTaskRecorder(store, projectDir, func() string { return "sess-1" })
	return r, store
}

func TestTaskRecorder_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	r, store := newRecorderForTest(t, dir)
	ctx := context.Background()

	r.RecordStart("task-1", "task", "demo")
	snap, err := store.GetTask(ctx, dir, "task-1")
	if err != nil || snap == nil {
		t.Fatalf("GetTask after start: %+v, %v", snap, err)
	}
	if snap.State != TaskStateRunning || snap.Version != 1 || snap.SessionID != "sess-1" {
		t.Fatalf("snapshot after start = %+v", snap)
	}

	r.RecordDone("task-1", jobs.Done, nil)
	snap, _ = store.GetTask(ctx, dir, "task-1")
	if snap.State != TaskStateSucceeded || snap.Version != 2 {
		t.Fatalf("snapshot after done = %+v", snap)
	}

	events, err := store.ListEvents(ctx, dir, "task-1", 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if events[0].EventType != "state_change" || events[0].State != TaskStateRunning || events[0].Sequence != 1 {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[1].State != TaskStateSucceeded || events[1].Sequence != 2 {
		t.Fatalf("event[1] = %+v", events[1])
	}
}

func TestTaskRecorder_FailedMapsError(t *testing.T) {
	dir := t.TempDir()
	r, store := newRecorderForTest(t, dir)
	ctx := context.Background()

	r.RecordStart("bash-1", "bash", "")
	r.RecordDone("bash-1", jobs.Failed, context.DeadlineExceeded)
	snap, _ := store.GetTask(ctx, dir, "bash-1")
	if snap.State != TaskStateFailed || snap.ErrorCode != "job_failed" || !strings.Contains(snap.ErrorSummary, "deadline") {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestTaskRecorder_TruncatesLongError(t *testing.T) {
	dir := t.TempDir()
	r, store := newRecorderForTest(t, dir)
	ctx := context.Background()

	long := strings.Repeat("x", maxErrorSummaryLen*2)
	r.RecordStart("t1", "bash", "")
	r.RecordDone("t1", jobs.Failed, fmt.Errorf("%s", long))
	snap, _ := store.GetTask(ctx, dir, "t1")
	if len(snap.ErrorSummary) > maxErrorSummaryLen {
		t.Fatalf("ErrorSummary length %d exceeds max %d", len(snap.ErrorSummary), maxErrorSummaryLen)
	}
}

func TestTaskRecorder_KilledAndInterruptedMapToCancelled(t *testing.T) {
	dir := t.TempDir()
	r, store := newRecorderForTest(t, dir)
	ctx := context.Background()

	r.RecordStart("t1", "task", "")
	r.RecordDone("t1", jobs.Killed, nil)
	snap, _ := store.GetTask(ctx, dir, "t1")
	if snap.State != TaskStateCancelled {
		t.Fatalf("killed -> %v, want cancelled", snap.State)
	}

	r.RecordStart("t2", "task", "")
	r.RecordDone("t2", jobs.Interrupted, nil)
	snap, _ = store.GetTask(ctx, dir, "t2")
	if snap.State != TaskStateCancelled {
		t.Fatalf("interrupted -> %v, want cancelled", snap.State)
	}
}

func TestTaskRecorder_RestartContinuesVersionAndKeepsCreatedAt(t *testing.T) {
	dir := t.TempDir()
	r, store := newRecorderForTest(t, dir)
	ctx := context.Background()

	// First lifecycle.
	r.RecordStart("task-1", "task", "")
	r.RecordDone("task-1", jobs.Done, nil)
	first, _ := store.GetTask(ctx, dir, "task-1")

	// Second lifecycle under the same id (job seq restarts per session).
	r.RecordStart("task-1", "task", "")
	second, _ := store.GetTask(ctx, dir, "task-1")
	if second.Version != first.Version+1 {
		t.Fatalf("version = %d, want %d", second.Version, first.Version+1)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt changed: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	if second.State != TaskStateRunning {
		t.Fatalf("state = %v, want running", second.State)
	}
}

func TestTaskRecorder_NonTerminalStatusDoesNotUpdate(t *testing.T) {
	dir := t.TempDir()
	r, store := newRecorderForTest(t, dir)
	ctx := context.Background()

	r.RecordStart("t1", "bash", "")
	r.RecordDone("t1", jobs.Running, nil) // never happens in practice; guard anyway
	snap, _ := store.GetTask(ctx, dir, "t1")
	if snap.State != TaskStateRunning || snap.Version != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestTaskRecorder_UnknownTaskDoneIsNoop(t *testing.T) {
	dir := t.TempDir()
	r, store := newRecorderForTest(t, dir)
	ctx := context.Background()

	r.RecordDone("never-started", jobs.Done, nil) // must not panic or write anything
	tasks, err := store.ListTasks(ctx, dir)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
}

func TestTaskRecorder_EmptySessionIDAllowed(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")
	r := NewTaskRecorder(store, dir, func() string { return "" })
	ctx := context.Background()

	r.RecordStart("t1", "bash", "")
	snap, err := store.GetTask(ctx, dir, "t1")
	if err != nil || snap == nil {
		t.Fatalf("GetTask: %+v, %v", snap, err)
	}
	events, err := store.ListEvents(ctx, dir, "t1", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("events: %+v, %v", events, err)
	}
}
