package main

import (
	"errors"
	"os"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
)

type snapshotErrorSessionController struct {
	control.SessionAPI
	err error
}

func (c *snapshotErrorSessionController) Snapshot() error { return c.err }

func TestTrashTopicSnapshotFailureKeepsRuntimeAndFiles(t *testing.T) {
	isolateDesktopUserDirs(t)
	projectRoot := t.TempDir()
	topicID := "topic_snapshot_failure"
	if err := addProject(projectRoot, ""); err != nil {
		t.Fatalf("add project: %v", err)
	}
	if err := setTopicTitle(projectRoot, topicID, "Snapshot failure"); err != nil {
		t.Fatalf("set topic title: %v", err)
	}
	dir := config.SessionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	sessionPath := writeTopicSessionWithPrompt(t, dir, "snapshot-failure.jsonl", topicID, "Snapshot failure", projectRoot, "preserve me", time.Now())
	base := controllerWithContent(t, sessionPath)
	defer base.Close()
	snapshotErr := errors.New("snapshot blocked")
	ctrl := &snapshotErrorSessionController{SessionAPI: base, err: snapshotErr}
	tab := &WorkspaceTab{ID: "snapshot-failure", Scope: "project", WorkspaceRoot: projectRoot, TopicID: topicID,
		TopicTitle: "Snapshot failure", SessionPath: sessionPath, Ctrl: ctrl, Ready: true, disabledMCP: map[string]ServerView{}}
	app := &App{tabs: map[string]*WorkspaceTab{tab.ID: tab}, tabOrder: []string{tab.ID}, activeTabID: tab.ID}
	if err := app.TrashTopic(topicID); !errors.Is(err, snapshotErr) {
		t.Fatalf("TrashTopic snapshot error = %v, want %v", err, snapshotErr)
	}
	if got := app.tabs[tab.ID]; got != tab || tab.removed {
		t.Fatalf("snapshot failure changed runtime binding: got=%p removed=%v", got, tab.removed)
	}
	if got := ctrl.SessionPath(); !sameDesktopPath(got, sessionPath) {
		t.Fatalf("snapshot failure session path = %q, want %q", got, sessionPath)
	}
	if agent.IsCleanupPending(sessionPath) {
		t.Fatal("snapshot failure must not publish a cleanup-pending marker")
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("snapshot failure removed the session file: %v", err)
	}
	if got := loadTopicTitle(projectRoot, topicID); got != "Snapshot failure" {
		t.Fatalf("snapshot failure topic title = %q", got)
	}
}

func TestTrashTopicRejectsConcurrentRuntimeMutationWithoutWaiting(t *testing.T) {
	isolateDesktopUserDirs(t)
	topicID := "topic_runtime_mutation_busy"
	if err := setTopicTitle("", topicID, "Runtime mutation busy"); err != nil {
		t.Fatalf("set topic title: %v", err)
	}
	app := &App{}
	app.runtimeRebuildMu.Lock()
	started := time.Now()
	err := app.TrashTopic(topicID)
	elapsed := time.Since(started)
	app.runtimeRebuildMu.Unlock()
	if !errors.Is(err, errTopicArchiveBusy) {
		t.Fatalf("TrashTopic error = %v, want %v", err, errTopicArchiveBusy)
	}
	if elapsed > time.Second {
		t.Fatalf("TrashTopic waited %s behind another runtime mutation", elapsed)
	}
	if got := loadTopicTitle("", topicID); got != "Runtime mutation busy" {
		t.Fatalf("busy archive changed topic title to %q", got)
	}
}
