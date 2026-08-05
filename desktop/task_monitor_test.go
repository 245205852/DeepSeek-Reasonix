package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/taskmonitor"
)

type taskKillController struct {
	control.SessionAPI
	mu     sync.Mutex
	killed []string
}

func (c *taskKillController) CancelJob(id string) bool {
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

func (c *taskKillController) killedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.killed...)
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
	projectA := t.TempDir()
	projectB := t.TempDir()
	pathA := filepath.Join(t.TempDir(), "shared-session.jsonl")
	pathB := filepath.Join(t.TempDir(), "shared-session.jsonl")
	ctrlA := &taskKillController{SessionAPI: control.New(control.Options{Label: "a", SessionPath: pathA})}
	ctrlB := &taskKillController{SessionAPI: control.New(control.Options{Label: "b", SessionPath: pathB})}
	defer ctrlA.Close()
	defer ctrlB.Close()

	app := &App{
		tabs: map[string]*WorkspaceTab{
			"active-a": {ID: "active-a", WorkspaceRoot: projectA, Ctrl: ctrlA},
		},
		detachedSessions: map[string]*WorkspaceTab{
			sessionRuntimeKey(pathB): {ID: "detached-b", WorkspaceRoot: projectB, Ctrl: ctrlB},
		},
		activeTabID: "active-a",
	}

	if agent.BranchID(pathA) != agent.BranchID(pathB) {
		t.Fatal("test setup must use colliding session IDs")
	}
	killer := desktopTaskJobKiller{app: app, projectDir: projectB}
	if !killer.Kill(agent.BranchID(pathB), "task-1") {
		t.Fatal("expected detached project B task to be killed")
	}
	if ctrlA.killCount() != 0 || ctrlB.killCount() != 1 {
		t.Fatalf("kill routed incorrectly: active=%d detached=%d", ctrlA.killCount(), ctrlB.killCount())
	}
}

func TestDesktopTaskJobKillerRefusesLegacyTaskWithoutSession(t *testing.T) {
	root := t.TempDir()
	path := agent.NewSessionPath(t.TempDir(), "session")
	ctrl := &taskKillController{SessionAPI: control.New(control.Options{Label: "session", SessionPath: path})}
	defer ctrl.Close()
	app := &App{tabs: map[string]*WorkspaceTab{"active": {ID: "active", WorkspaceRoot: root, Ctrl: ctrl}}}

	if (desktopTaskJobKiller{app: app, projectDir: root}).Kill("", "task-1") {
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

func TestStopTaskRoutesMonitorIdentityToRuntimeJob(t *testing.T) {
	root := t.TempDir()
	path := agent.NewSessionPath(t.TempDir(), "session")
	ctrl := &taskKillController{SessionAPI: control.New(control.Options{Label: "session", SessionPath: path})}
	defer ctrl.Close()
	app := &App{
		ctx: context.Background(),
		tabs: map[string]*WorkspaceTab{
			"active": {ID: "active", Scope: "project", WorkspaceRoot: root, Ctrl: ctrl},
		},
		activeTabID: "active",
	}
	sessionID := agent.BranchID(path)
	monitorID := sessionID + "--task-1"
	now := time.Now()
	if err := app.taskStore().SaveTask(app.ctx, root, taskmonitor.TaskSnapshot{
		SchemaVersion: 1, TaskID: monitorID, JobID: "task-1", SessionID: sessionID,
		State: taskmonitor.TaskStateRunning, RuntimeState: taskmonitor.RuntimeStateAlive,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := app.StopTask(monitorID, 1, "", "desktop-route")
	if err != nil || !res.Accepted {
		t.Fatalf("StopTask: result=%+v err=%v", res, err)
	}
	ids := ctrl.killedIDs()
	if len(ids) != 1 || ids[0] != "task-1" {
		t.Fatalf("runtime killed IDs = %v, want [task-1]", ids)
	}
}
