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

func TestTmuxAdapterAcceptsCleanableProjectDir(t *testing.T) {
	parent := t.TempDir()
	for _, projectDir := range []string{
		filepath.Join(parent, "nested", "..", "project"),
		filepath.Join(parent, "project..archive"),
	} {
		if err := os.MkdirAll(filepath.Clean(projectDir), 0o755); err != nil {
			t.Fatal(err)
		}
		s := NewInMemoryStore()
		seedTmuxTask(t, s, projectDir)
		a := NewTmuxAdapterWithRunner(s, ".reasonix/tasks", &tmuxMock{})
		if result := a.Attach(context.Background(), projectDir, "t1", "demo"); result.Error != nil {
			t.Fatalf("Attach(%q): %+v", projectDir, result.Error)
		}
	}
}

func TestTmuxAdapterRejectsSymlinkMappingDirectory(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	root := filepath.Join(project, ".reasonix", "tasks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".tmux")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	s := NewInMemoryStore()
	seedTmuxTask(t, s, project)
	attachRunner := &tmuxMock{}
	a := NewTmuxAdapterWithRunner(s, ".reasonix/tasks", attachRunner)
	if result := a.Attach(context.Background(), project, "t1", "demo"); result.Error == nil || result.Error.Code != ErrTmuxMappingFailed {
		t.Fatalf("expected mapping write rejection, got %+v", result)
	}
	outsideMapping := filepath.Join(outside, "t1.json")
	if _, err := os.Stat(outsideMapping); !os.IsNotExist(err) {
		t.Fatalf("mapping escaped through symlink: %v", err)
	}

	mapping := `{"schema_version":1,"task_id":"t1","session":"victim","window":"task","pane":"victim:task.0"}`
	if err := os.WriteFile(outsideMapping, []byte(mapping), 0o600); err != nil {
		t.Fatal(err)
	}
	readRunner := &tmuxMock{}
	a = NewTmuxAdapterWithRunner(s, ".reasonix/tasks", readRunner)
	for _, result := range []TmuxResult{
		a.Status(context.Background(), project, "t1"),
		a.Detach(context.Background(), project, "t1"),
	} {
		if result.Error == nil || result.Error.Code != ErrTmuxMappingFailed {
			t.Fatalf("expected mapping read rejection, got %+v", result)
		}
	}
	if len(readRunner.calls) != 0 {
		t.Fatalf("untrusted mapping triggered tmux calls: %#v", readRunner.calls)
	}
	if _, err := os.Stat(outsideMapping); err != nil {
		t.Fatalf("outside mapping was removed: %v", err)
	}
}

func TestTmuxAdapterWritesPrivateMappingFiles(t *testing.T) {
	project := t.TempDir()
	s := NewInMemoryStore()
	seedTmuxTask(t, s, project)
	a := NewTmuxAdapterWithRunner(s, ".reasonix/tasks", &tmuxMock{})
	if result := a.Attach(context.Background(), project, "t1", "demo"); result.Error != nil {
		t.Fatal(result.Error)
	}

	for path, want := range map[string]os.FileMode{
		filepath.Join(project, ".reasonix", "tasks"):                     0o700,
		filepath.Join(project, ".reasonix", "tasks", ".tmux"):            0o700,
		filepath.Join(project, ".reasonix", "tasks", ".tmux", "t1.json"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}
