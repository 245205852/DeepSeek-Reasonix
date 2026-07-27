package taskmonitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileStore_ListTasks_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")

	tasks, err := store.ListTasks(context.Background(), dir)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected empty, got %d", len(tasks))
	}
}

func TestFileStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")

	// Write a task snapshot
	taskDir := filepath.Join(dir, ".reasonix", "tasks", "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	snap := TaskSnapshot{
		SchemaVersion: 1, TaskID: "task-1", SessionID: "s1",
		State: TaskStateFailed, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		ErrorCode: "TIMEOUT", ErrorSummary: "deadline exceeded",
	}
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(filepath.Join(taskDir, "snapshot.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Read back
	got, err := store.GetTask(context.Background(), dir, "task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if got.TaskID != "task-1" || got.ErrorCode != "TIMEOUT" {
		t.Errorf("mismatch: %+v", got)
	}
}

func TestFileStore_ListEvents_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")

	taskDir := filepath.Join(dir, ".reasonix", "tasks", "t1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write events as JSONL
	events := `{"sequence":1,"timestamp":"2025-01-01T00:00:01Z","event_type":"state_change","task_id":"t1","session_id":"s","state":"queued"}
{"sequence":2,"timestamp":"2025-01-01T00:00:02Z","event_type":"state_change","task_id":"t1","session_id":"s","state":"running"}
{"sequence":3,"timestamp":"2025-01-01T00:00:03Z","event_type":"error","task_id":"t1","session_id":"s","state":"failed","error_code":"E1"}
`
	if err := os.WriteFile(filepath.Join(taskDir, "events.jsonl"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListEvents(context.Background(), dir, "t1", 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 events, got %d", len(got))
	}
	if got[2].ErrorCode != "E1" {
		t.Errorf("expected E1, got %q", got[2].ErrorCode)
	}
}

func TestFileStore_ListEvents_AfterCursor(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")
	taskDir := filepath.Join(dir, ".reasonix", "tasks", "t1")
	os.MkdirAll(taskDir, 0o755)
	events := `{"sequence":1,"timestamp":"2025-01-01T00:00:01Z","event_type":"e","task_id":"t1","session_id":"s","state":"queued"}
{"sequence":2,"timestamp":"2025-01-01T00:00:02Z","event_type":"e","task_id":"t1","session_id":"s","state":"running"}
`
	os.WriteFile(filepath.Join(taskDir, "events.jsonl"), []byte(events), 0o644)

	got, _ := store.ListEvents(context.Background(), dir, "t1", 1)
	if len(got) != 1 || got[0].Sequence != 2 {
		t.Errorf("expected [seq=2], got %d events, seq=%d", len(got), got[0].Sequence)
	}
}

func TestFileStore_RejectsPathTraversal_TaskID(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")

	_, err := store.GetTask(context.Background(), dir, "../escape")
	if err == nil || !strings.Contains(err.Error(), "path separator") {
		t.Fatalf("expected path traversal rejection, got %v", err)
	}
}

func TestFileStore_RejectsPathTraversal_ProjectDir(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")

	_, err := store.ListTasks(context.Background(), dir+"/../../../etc")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected escape rejection, got %v", err)
	}
}

func TestFileStore_RejectsEmptyTaskID(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")

	_, err := store.GetTask(context.Background(), dir, "")
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("expected empty rejection, got %v", err)
	}
}

func TestFileStore_RejectsDotTaskID(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(".reasonix/tasks")

	_, err := store.GetTask(context.Background(), dir, ".")
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected rejection for '.', got %v", err)
	}
}
