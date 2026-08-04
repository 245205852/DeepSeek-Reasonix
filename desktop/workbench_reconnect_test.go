package main

import (
	"strings"
	"testing"

	"reasonix/internal/remote/protocol"
	"reasonix/internal/remote/workbench/reconnect"
	"reasonix/internal/remote/workbench/target"
)

func TestRemoteMutationBlockedDuringReconnect(t *testing.T) {
	a := &App{workbenchKernel: newWorkbenchKernel()}
	k := a.workbench()
	_, gen, err := k.targets.BeginRemoteConnect("lab", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.targets.MarkRemoteConnected(gen); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := k.targets.ActivateRemote(gen); err != nil {
		t.Fatal(err)
	}
	k.targets.SetRemoteSession(gen, protocol.RuntimeTarget{WorkspaceID: "ws", SessionID: "sess"}, "epoch-1", "fp")
	k.mu.Lock()
	historyText := "cached remote transcript"
	k.remoteGen = gen
	k.snapshot = protocol.SessionSnapshot{
		Target:       protocol.RuntimeTarget{WorkspaceID: "ws", SessionID: "sess"},
		RuntimeEpoch: "epoch-1",
		History: protocol.HistoryPage{
			Messages: []protocol.HistoryMessage{{Role: "assistant", Content: &historyText}},
			EndTurn:  1, TotalTurns: 1,
		},
		Meta: protocol.SessionMetaSnapshot{ResolvedProfile: protocol.ResolvedProfile{Model: "provider/model"}},
	}
	k.catalog = protocol.WorkspaceCatalogResult{Models: []protocol.ModelCatalogItem{{Ref: "provider/model", Provider: "provider", Model: "model"}}}
	k.mu.Unlock()

	id, _, _, changed := k.targets.MarkRemoteDisconnected(gen)
	if !changed || id.Kind != target.KindRemote {
		t.Fatalf("unexpected disconnect result: %+v changed=%v", id, changed)
	}
	k.mu.Lock()
	k.remote = nil
	k.remoteGen = 0
	k.mu.Unlock()

	if a.remoteRoute() != remoteRouteUnavailable {
		t.Fatalf("route = %v, want unavailable", a.remoteRoute())
	}
	if err := a.remoteMutationBlocked(); err == nil || !strings.Contains(err.Error(), "REMOTE_RECONNECTING") {
		t.Fatalf("mutation blocked = %v", err)
	}
	// Cache projection still available for transcript reads.
	snapshot, _, _, ok := a.projectedRemoteWorkbench()
	if !ok || snapshot.Target.SessionID != "sess" {
		t.Fatalf("projected cache lost: ok=%v snapshot=%+v", ok, snapshot)
	}
	// Live client path must not report connected.
	if _, _, _, _, ok := a.connectedRemoteWorkbench(); ok {
		t.Fatal("connected client should be unavailable")
	}
	// Submit helper must not fall through to Local.
	handled, err := a.workbenchSubmit("hello", "hello", "", nil, false)
	if !handled || err == nil || !strings.Contains(err.Error(), "REMOTE_RECONNECTING") {
		t.Fatalf("submit handled=%v err=%v", handled, err)
	}
	for name, call := range map[string]func() error{
		"shell":          func() error { return a.RunShellForTab("", "pwd") },
		"new session":    func() error { return a.NewSessionForTab("") },
		"clear session":  func() error { return a.ClearSessionForTab("") },
		"compact":        func() error { return a.CompactForTab("") },
		"rewind":         func() error { return a.RewindForTab("", 1, "both") },
		"summarize from": func() error { return a.SummarizeFromForTab("", 1) },
		"summarize to":   func() error { return a.SummarizeUpToForTab("", 1) },
		"goal":           func() error { return a.SetGoalForTab("", "ship") },
		"token mode":     func() error { return a.SetTokenModeForTab("", "save") },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "REMOTE_RECONNECTING") {
			t.Errorf("%s fell through instead of failing closed: %v", name, err)
		}
	}
	if history := a.HistoryForTab(""); len(history) != 1 || history[0].Content != historyText {
		t.Fatalf("cached history = %+v", history)
	}
	if models := a.ModelsForTab(""); len(models) != 1 || models[0].Ref != "provider/model" {
		t.Fatalf("cached models = %+v", models)
	}
	if preview := a.ReadFileForTab("", "README.md"); !strings.Contains(preview.Err, "REMOTE_RECONNECTING") {
		t.Fatalf("remote file preview fell through: %+v", preview)
	}
	if preview := a.PreviewRewindForTab("", 1, "both"); !strings.Contains(preview.Error, "REMOTE_RECONNECTING") {
		t.Fatalf("remote rewind preview fell through: %+v", preview)
	}
	if result := a.CommitRewindForTab("", "", 1, "both"); !strings.Contains(result.Error, "REMOTE_RECONNECTING") {
		t.Fatalf("remote rewind commit fell through: %+v", result)
	}
	if result := a.UndoRewindForTab("", "txn"); !strings.Contains(result.Error, "REMOTE_RECONNECTING") {
		t.Fatalf("remote rewind undo fell through: %+v", result)
	}
	if preview := a.PreviewWorkspaceFileRevertForTab("", "README.md"); !strings.Contains(preview.Error, "REMOTE_RECONNECTING") {
		t.Fatalf("remote file-revert preview fell through: %+v", preview)
	}
	if result := a.CommitWorkspaceFileRevertForTab("", "plan", "keep_current"); !strings.Contains(result.Error, "REMOTE_RECONNECTING") {
		t.Fatalf("remote file-revert commit fell through: %+v", result)
	}
	if _, err := a.ForkForTab("", 1); err == nil || !strings.Contains(err.Error(), "CAPABILITY_UNAVAILABLE") {
		t.Fatalf("remote fork fell through: %v", err)
	}
}

func TestWorkbenchActiveTargetIncludesReconnectFields(t *testing.T) {
	a := &App{workbenchKernel: newWorkbenchKernel()}
	k := a.workbench()
	_, gen, _ := k.targets.BeginRemoteConnect("lab", "/srv")
	_ = k.targets.MarkRemoteConnected(gen)
	_, _, _, _ = k.targets.ActivateRemote(gen)
	_, _, _, _ = k.targets.MarkRemoteDisconnected(gen)
	k.targets.UpdateReconnectAttempt(k.targets.Remote().LogicalGen, 3, "connection reset")

	active := a.WorkbenchActiveTarget()
	if active["kind"] != "ssh" {
		t.Fatalf("kind = %v", active["kind"])
	}
	if active["state"] != string(target.StateReconnecting) {
		t.Fatalf("state = %v", active["state"])
	}
	if active["attempt"] != 3 {
		t.Fatalf("attempt = %v", active["attempt"])
	}
	if active["retryable"] != true {
		t.Fatalf("retryable = %v", active["retryable"])
	}
}

func TestSwitchLocalDoesNotClearRemoteSlot(t *testing.T) {
	a := &App{workbenchKernel: newWorkbenchKernel()}
	k := a.workbench()
	_, gen, _ := k.targets.BeginRemoteConnect("lab", "/srv")
	_ = k.targets.MarkRemoteConnected(gen)
	_, _, _, _ = k.targets.ActivateRemote(gen)
	k.targets.SetRemoteSession(gen, protocol.RuntimeTarget{WorkspaceID: "ws", SessionID: "sess"}, "epoch-1", "fp")
	_ = a.WorkbenchSwitchLocal()
	if remote := k.targets.Remote(); remote == nil || !remote.Connected {
		// After switch local while still connected, remote stays as background.
		t.Fatalf("remote slot lost after switch local: %+v", remote)
	}
	id, _, _ := k.targets.Active()
	if id.Kind != target.KindLocal {
		t.Fatalf("active = %+v", id)
	}
}

func TestClassifyReconnectErrors(t *testing.T) {
	if reconnect.ClassifyError(reconnect.ErrTargetMissing) != reconnect.ClassStop {
		t.Fatal("target missing must stop")
	}
	if reconnect.ClassifyError(nil) != reconnect.ClassRetry {
		t.Fatal("nil classifies as retry")
	}
}
