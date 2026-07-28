package taskmonitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type tmuxMock struct {
	calls [][]string
	err   error
}

func (m *tmuxMock) Run(_ context.Context, args ...string) ([]byte, error) {
	m.calls = append(m.calls, append([]string(nil), args...))
	return nil, m.err
}

func seedTmuxTask(t *testing.T, s *InMemoryStore, projectDir string) {
	t.Helper()
	if err := s.UpsertTask(projectDir, TaskSnapshot{SchemaVersion: 1, TaskID: "t1", SessionID: "s1", State: TaskStateRunning, Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestTmuxAdapterAttachIdempotent(t *testing.T) {
	s := NewInMemoryStore()
	root := t.TempDir()
	seedTmuxTask(t, s, root)
	mock := &tmuxMock{}
	a := NewTmuxAdapterWithRunner(s, ".reasonix/tasks", mock)
	first := a.Attach(context.Background(), root, "t1", "demo")
	if first.Error != nil || first.Mapping == nil {
		t.Fatalf("attach failed: %+v", first)
	}
	second := a.Attach(context.Background(), root, "t1", "demo")
	if !second.Idempotent {
		t.Fatalf("expected idempotent attach: %+v", second)
	}
	if len(mock.calls) != 2 { // new-session, then has-session
		t.Fatalf("unexpected tmux calls: %#v", mock.calls)
	}
}

func TestTmuxAdapterUnavailableDoesNotChangeTask(t *testing.T) {
	s := NewInMemoryStore()
	root := t.TempDir()
	seedTmuxTask(t, s, root)
	a := NewTmuxAdapterWithRunner(s, ".reasonix/tasks", nil)
	r := a.Attach(context.Background(), root, "t1", "")
	if r.Error == nil || r.Error.Code != ErrTmuxUnavailable {
		t.Fatalf("expected unavailable, got %+v", r)
	}
	snap, _ := s.GetTask(context.Background(), root, "t1")
	if snap == nil || snap.State != TaskStateRunning {
		t.Fatal("tmux operation changed task state")
	}
}

func TestTmuxAdapterRejectsUnsafeNames(t *testing.T) {
	s := NewInMemoryStore()
	seedTmuxTask(t, s, "/p")
	a := NewTmuxAdapterWithRunner(s, ".reasonix/tasks", &tmuxMock{})
	r := a.Attach(context.Background(), "/p", "t1", "bad/name")
	if r.Error == nil || r.Error.Code != ErrTmuxInvalidName {
		t.Fatalf("expected invalid name, got %+v", r)
	}
}

func TestTmuxAdapterStaleAndDetach(t *testing.T) {
	s := NewInMemoryStore()
	root := t.TempDir()
	seedTmuxTask(t, s, root)
	mock := &tmuxMock{}
	a := NewTmuxAdapterWithRunner(s, ".reasonix/tasks", mock)
	if r := a.Attach(context.Background(), root, "t1", "demo"); r.Error != nil {
		t.Fatal(r.Error)
	}
	mock.err = os.ErrNotExist
	r := a.Status(context.Background(), root, "t1")
	if r.Mapping == nil || !r.Mapping.Stale {
		t.Fatalf("expected stale mapping: %+v", r)
	}
	mock.err = nil
	if r := a.Detach(context.Background(), root, "t1"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if _, err := os.Stat(filepath.Join(root, ".reasonix/tasks/.tmux/t1.json")); !os.IsNotExist(err) {
		t.Fatalf("mapping was not removed: %v", err)
	}
}

func TestTmuxAdapterRejectsTaskPathTraversal(t *testing.T) {
	a := NewTmuxAdapterWithRunner(NewInMemoryStore(), ".reasonix/tasks", &tmuxMock{})
	r := a.Status(context.Background(), t.TempDir(), "../secret")
	if r.Error == nil {
		t.Fatal("expected invalid task id error")
	}
}
