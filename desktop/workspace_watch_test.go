package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceChangeHubSharesRootRevisionsAndIsolatesSessions(t *testing.T) {
	root := t.TempDir()
	app := &App{tabs: map[string]*WorkspaceTab{}}
	app.workspaceHub = newWorkspaceChangeHub(app)
	t.Cleanup(func() { app.workspaceHub.close() })
	app.tabs["a"] = &WorkspaceTab{ID: "a", WorkspaceRoot: root}
	app.tabs["b"] = &WorkspaceTab{ID: "b", WorkspaceRoot: root}

	beforeA := app.WorkspaceRevisionForTab("a")
	beforeB := app.WorkspaceRevisionForTab("b")
	app.workspaceHub.observeAgentMutation("a", []string{"pkg/main.go"}, false)
	afterA := app.WorkspaceRevisionForTab("a")
	afterB := app.WorkspaceRevisionForTab("b")
	if afterA.Revisions.Content <= beforeA.Revisions.Content || afterB.Revisions.Content != afterA.Revisions.Content {
		t.Fatalf("root content revision not shared: before=%+v afterA=%+v afterB=%+v", beforeA, afterA, afterB)
	}
	if afterA.Revisions.Session <= beforeA.Revisions.Session || afterB.Revisions.Session != beforeB.Revisions.Session {
		t.Fatalf("session revision leaked across tabs: beforeA=%+v beforeB=%+v afterA=%+v afterB=%+v", beforeA, beforeB, afterA, afterB)
	}
}

func TestWorkspaceChangeHubCapsOpaqueMutation(t *testing.T) {
	root := t.TempDir()
	app := &App{tabs: map[string]*WorkspaceTab{"a": {ID: "a", WorkspaceRoot: root}}}
	app.workspaceHub = newWorkspaceChangeHub(app)
	t.Cleanup(func() { app.workspaceHub.close() })
	app.workspaceHub.observeAgentMutation("a", nil, true)
	key := canonicalWorkspaceRoot(root)
	app.workspaceHub.mu.Lock()
	r := app.workspaceHub.roots[key]
	allPaths := r != nil && r.allPaths
	app.workspaceHub.mu.Unlock()
	if !allPaths {
		t.Fatal("opaque mutation did not become allPaths invalidation")
	}
}

func TestWorkspaceChangeHubFilesystemWritePublishesContentRevision(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{tabs: map[string]*WorkspaceTab{"a": {ID: "a", WorkspaceRoot: root}}}
	app.workspaceHub = newWorkspaceChangeHub(app)
	t.Cleanup(func() { app.workspaceHub.close() })
	initial := app.WorkspaceRevisionForTab("a").Revisions.Content
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The watcher callback is asynchronous; wait without imposing a fixed
	// sleep so slow CI filesystems get the same bounded opportunity.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.WorkspaceRevisionForTab("a").Revisions.Content > initial {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("filesystem write did not advance content revision")
}
