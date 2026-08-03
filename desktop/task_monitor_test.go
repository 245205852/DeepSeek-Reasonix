package main

import (
	"sync"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/taskmonitor"
)

type taskKillController struct {
	control.SessionAPI
	mu     sync.Mutex
	killed []string
}

func (c *taskKillController) KillJob(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.killed = append(c.killed, id)
	return true
}

func (c *taskKillController) killCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.killed)
}

func TestTaskControlConcurrentInitializationReturnsOneService(t *testing.T) {
	app := &App{}
	const callers = 16
	services := make(chan *taskmonitor.ControlService, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			services <- app.taskControl()
		}()
	}
	wg.Wait()
	close(services)

	var first *taskmonitor.ControlService
	for service := range services {
		if first == nil {
			first = service
			continue
		}
		if service != first {
			t.Fatal("taskControl returned more than one process-wide service")
		}
	}
}

func TestDesktopTaskJobKillerRoutesBySessionNotActiveTab(t *testing.T) {
	pathA := agent.NewSessionPath(t.TempDir(), "session-a")
	pathB := agent.NewSessionPath(t.TempDir(), "session-b")
	ctrlA := &taskKillController{SessionAPI: control.New(control.Options{Label: "a", SessionPath: pathA})}
	ctrlB := &taskKillController{SessionAPI: control.New(control.Options{Label: "b", SessionPath: pathB})}
	defer ctrlA.Close()
	defer ctrlB.Close()

	app := &App{
		tabs: map[string]*WorkspaceTab{
			"active-a": {ID: "active-a", Ctrl: ctrlA},
		},
		detachedSessions: map[string]*WorkspaceTab{
			sessionRuntimeKey(pathB): {ID: "detached-b", Ctrl: ctrlB},
		},
		activeTabID: "active-a",
	}

	killer := desktopTaskJobKiller{app: app}
	if !killer.Kill(agent.BranchID(pathB), "task-1") {
		t.Fatal("expected detached session task to be killed")
	}
	if ctrlA.killCount() != 0 || ctrlB.killCount() != 1 {
		t.Fatalf("kill routed incorrectly: active=%d detached=%d", ctrlA.killCount(), ctrlB.killCount())
	}
}

func TestDesktopTaskJobKillerRefusesLegacyTaskWithoutSession(t *testing.T) {
	path := agent.NewSessionPath(t.TempDir(), "session")
	ctrl := &taskKillController{SessionAPI: control.New(control.Options{Label: "session", SessionPath: path})}
	defer ctrl.Close()
	app := &App{tabs: map[string]*WorkspaceTab{"active": {ID: "active", Ctrl: ctrl}}}

	if (desktopTaskJobKiller{app: app}).Kill("", "task-1") {
		t.Fatal("legacy task without session ID must not be routed by colliding task ID")
	}
	if ctrl.killCount() != 0 {
		t.Fatalf("legacy task unexpectedly killed %d runtime(s)", ctrl.killCount())
	}
}

func TestTaskMonitorUsesActiveWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	app := &App{
		tabs: map[string]*WorkspaceTab{
			"active": {ID: "active", Scope: "project", WorkspaceRoot: root},
		},
		activeTabID: "active",
	}
	if got := app.projectDir(); got != root {
		t.Fatalf("projectDir = %q, want active workspace %q", got, root)
	}
}
