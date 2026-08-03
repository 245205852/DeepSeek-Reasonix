package taskmonitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
	if snap.State != TaskStateRunning || snap.RuntimeState != RuntimeStateAlive || snap.Version != 1 || snap.SessionID != "sess-1" {
		t.Fatalf("snapshot after start = %+v", snap)
	}

	r.RecordDone("task-1", jobs.Done, nil)
	snap, _ = store.GetTask(ctx, dir, "task-1")
	if snap.State != TaskStateSucceeded || snap.RuntimeState != RuntimeStateExited || snap.Version != 2 {
		t.Fatalf("snapshot after done = %+v", snap)
	}

	events, err := store.ListEvents(ctx, dir, "task-1", 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if events[0].EventType != "state_change" || events[0].State != TaskStateRunning || events[0].RuntimeState != RuntimeStateAlive || events[0].Sequence != 1 {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[1].State != TaskStateSucceeded || events[1].RuntimeState != RuntimeStateExited || events[1].Sequence != 2 {
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
	if second.State != TaskStateRunning || second.RuntimeState != RuntimeStateAlive {
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

type blockingExitedSaveStore struct {
	WriteStore
	once    sync.Once
	blocked chan struct{}
	release chan struct{}
}

func (s *blockingExitedSaveStore) SaveTask(ctx context.Context, projectDir string, snap TaskSnapshot) error {
	if snap.RuntimeState == RuntimeStateExited {
		s.once.Do(func() {
			close(s.blocked)
			<-s.release
		})
	}
	return s.WriteStore.SaveTask(ctx, projectDir, snap)
}

func TestTaskRecorder_DoneRetriesAfterConcurrentControlUpdate(t *testing.T) {
	base := NewInMemoryStore()
	now := time.Now()
	if err := base.UpsertTask("/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "task-1", SessionID: "session-1",
		State: TaskStateRunning, RuntimeState: RuntimeStateAlive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	store := &blockingExitedSaveStore{
		WriteStore: base,
		blocked:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	recorder := NewTaskRecorder(store, "/p", func() string { return "session-1" })
	done := make(chan struct{})
	go func() {
		recorder.RecordDone("task-1", jobs.Killed, nil)
		close(done)
	}()
	<-store.blocked

	control := NewControlService(base)
	res, err := control.StopTask(context.Background(), "/p", "task-1", 1, "", "")
	if err != nil || !res.Accepted {
		t.Fatalf("concurrent stop: result=%+v err=%v", res, err)
	}
	close(store.release)
	<-done

	snap, err := base.GetTask(context.Background(), "/p", "task-1")
	if err != nil || snap == nil {
		t.Fatalf("GetTask: snap=%+v err=%v", snap, err)
	}
	if snap.State != TaskStateCancelled || snap.RuntimeState != RuntimeStateExited || snap.Version != 3 {
		t.Fatalf("completion evidence was lost after CAS retry: %+v", snap)
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
