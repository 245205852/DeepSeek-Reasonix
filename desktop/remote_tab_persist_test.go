package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/config"
)

func readPersistedTabsFile(t *testing.T) desktopTabsFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(config.ReasonixHomeDir(), tabsFileName))
	if err != nil {
		t.Fatalf("read tabs file: %v", err)
	}
	var f desktopTabsFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse tabs file: %v", err)
	}
	return f
}

func seedLocalTab(a *App, id string) {
	a.mu.Lock()
	if a.tabs == nil {
		a.tabs = map[string]*WorkspaceTab{}
	}
	a.tabs[id] = &WorkspaceTab{ID: id, Scope: "global"}
	a.tabOrder = append(a.tabOrder, id)
	a.mu.Unlock()
}

// TestRemoteTabOpenPersistRoundTrip: an open remote tab lands in
// desktop-tabs.json; closing removes it again.
func TestRemoteTabOpenPersistRoundTrip(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)

	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")

	f := readPersistedTabsFile(t)
	if len(f.RemoteTabs) != 1 || len(f.RemoteTabOrder) != 1 {
		t.Fatalf("persisted remote section = %+v / %v, want one entry", f.RemoteTabs, f.RemoteTabOrder)
	}
	entry := f.RemoteTabs[0]
	if entry.ID != meta.ID || entry.HostID != "box" || entry.Workspace != "~/app" {
		t.Fatalf("persisted entry = %+v, want id/host/workspace for %s", entry, meta.ID)
	}
	if f.RemoteTabOrder[0] != entry.ID {
		t.Fatalf("persisted remote order = %v, want the entry id first", f.RemoteTabOrder)
	}
	if f.ActiveTab != meta.ID {
		t.Fatalf("persisted active tab = %q, want the active remote id", f.ActiveTab)
	}

	if err := a.CloseRemoteTab(meta.ID); err != nil {
		t.Fatal(err)
	}
	if f = readPersistedTabsFile(t); len(f.RemoteTabs) != 0 {
		t.Fatalf("closed tab still persisted: %+v", f.RemoteTabs)
	}
}

// TestRemoteTabRestoreBuildsDisconnectedShells: restore rebuilds shells
// without connecting anything; invalid and local-colliding ids are skipped;
// the persisted active remote id routes to the shell.
func TestRemoteTabRestoreBuildsDisconnectedShells(t *testing.T) {
	seedBridgeTestHost(t, "box")
	a := &App{}
	seedLocalTab(a, "local-1")
	f := desktopTabsFile{
		RemoteTabs: []desktopRemoteTabEntry{
			{ID: "r-1", HostID: "box", Workspace: "~/app", TopicTitle: "Fix bug"},
			{ID: "r-2", HostID: "box", Workspace: "~/web"},
			{ID: "", HostID: "box", Workspace: "~/skip"},
			{ID: "local-1", HostID: "box", Workspace: "~/dup"},
		},
		RemoteTabOrder: []string{"r-2", "r-1"},
		ActiveTab:      "r-1",
	}
	a.restoreRemoteTabShells(f)

	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	if len(a.remoteTabs) != 2 || a.remoteTabs["r-1"] == nil || a.remoteTabs["r-2"] == nil {
		t.Fatalf("restored shells = %+v", a.remoteTabs)
	}
	for id, tab := range a.remoteTabs {
		if tab.state != "disconnected" || !tab.session.newSession {
			t.Fatalf("shell %s = state %q newSession %v, want disconnected + newSession", id, tab.state, tab.session.newSession)
		}
		if tab.client != nil || tab.cancel != nil {
			t.Fatalf("shell %s connected during restore", id)
		}
	}
	if got := a.remoteTabs["r-1"].topicTitle; got != "Fix bug" {
		t.Fatalf("restored title = %q, want the persisted one", got)
	}
	if got := strings.Join(a.remoteTabLayout.order, ","); got != "r-2,r-1" {
		t.Fatalf("restored remote order = %q, want r-2,r-1", got)
	}
	if a.remoteTabLayout.activeID != "r-1" {
		t.Fatalf("remoteActiveTabID = %q, want r-1", a.remoteTabLayout.activeID)
	}
}

// TestActivateDisconnectedShellReconnects: clicking a shell (SetActiveTab)
// drives the bootstrap — connect, serve, POST /new — reusing the shell id.
func TestActivateDisconnectedShellReconnects(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	a.remoteTabMu.Lock()
	a.remoteTabs = map[string]*remoteTab{
		"shell-1": {id: "shell-1", ref: RemoteTabRef{HostID: "box", Workspace: "~/app"}, state: "disconnected", session: remoteTabSessionState{newSession: true}, hostLabel: "box", topicTitle: "app"},
	}
	a.remoteTabLayout.order = []string{"shell-1"}
	a.remoteTabMu.Unlock()

	if err := a.SetActiveTab("shell-1"); err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, "shell-1", "ready")
	newCalled, _, _ := fs.snapshot()
	if newCalled != 1 {
		t.Fatalf("POST /new called %d times, want 1 (fresh blank session)", newCalled)
	}
	a.remoteTabMu.Lock()
	active := a.remoteTabLayout.activeID
	a.remoteTabMu.Unlock()
	if active != "shell-1" {
		t.Fatalf("remoteActiveTabID = %q, want shell-1", active)
	}
}

func TestSetActiveRemoteTabPersistsAndUnknownKeepsSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	a := &App{}
	seedLocalTab(a, "local-1")
	a.mu.Lock()
	a.activeTabID = "local-1"
	a.mu.Unlock()
	a.remoteTabMu.Lock()
	a.remoteTabs = map[string]*remoteTab{
		"remote-1": {id: "remote-1", ref: RemoteTabRef{HostID: "box", Workspace: "~/app"}, state: "ready"},
	}
	a.remoteTabLayout.order = []string{"remote-1"}
	a.remoteTabMu.Unlock()

	if err := a.SetActiveTab("remote-1"); err != nil {
		t.Fatal(err)
	}
	if got := readPersistedTabsFile(t).ActiveTab; got != "remote-1" {
		t.Fatalf("persisted active tab = %q, want remote-1", got)
	}
	if err := a.SetActiveTab("missing"); err == nil {
		t.Fatal("unknown tab activation succeeded")
	}
	a.remoteTabMu.Lock()
	active := a.remoteTabLayout.activeID
	a.remoteTabMu.Unlock()
	if active != "remote-1" {
		t.Fatalf("unknown activation cleared remote selection: %q", active)
	}
}

// TestOpenRemoteProjectTabRevivesShell: the tree-group path (ensure-open)
// reconnects a disconnected shell in place instead of only activating it.
func TestOpenRemoteProjectTabRevivesShell(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	a.remoteTabMu.Lock()
	a.remoteTabs = map[string]*remoteTab{
		"shell-1": {id: "shell-1", ref: RemoteTabRef{HostID: "box", Workspace: "~/app"}, state: "disconnected", session: remoteTabSessionState{newSession: true}, hostLabel: "box", topicTitle: "app"},
	}
	a.remoteTabLayout.order = []string{"shell-1"}
	a.remoteTabMu.Unlock()

	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "shell-1" {
		t.Fatalf("revived tab id = %q, want the shell id shell-1", meta.ID)
	}
	waitForTabState(t, a, "shell-1", "ready")
}

// TestReorderTabsMixedPersistsBothOrders: the full strip order partitions into
// local and remote orders; both persist; unknown remote ids reject the whole
// reorder without mutating either side.
func TestReorderTabsMixedPersistsBothOrders(t *testing.T) {
	seedBridgeTestHost(t, "box")
	a := &App{}
	seedLocalTab(a, "l1")
	seedLocalTab(a, "l2")
	a.remoteTabMu.Lock()
	a.remoteTabs = map[string]*remoteTab{"r1": {id: "r1", ref: RemoteTabRef{HostID: "box", Workspace: "~/app"}}}
	a.remoteTabLayout.order = []string{"r1"}
	a.remoteTabMu.Unlock()

	if err := a.ReorderTabs([]string{"r1", "l2", "l1"}); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	order := append([]string(nil), a.tabOrder...)
	a.mu.RUnlock()
	if len(order) != 2 || order[0] != "l2" || order[1] != "l1" {
		t.Fatalf("local order = %v, want [l2 l1]", order)
	}
	f := readPersistedTabsFile(t)
	if len(f.RemoteTabOrder) != 1 || f.RemoteTabOrder[0] != "r1" {
		t.Fatalf("persisted remote order = %v, want [r1]", f.RemoteTabOrder)
	}
	if got := strings.Join(f.TabOrder, ","); got != "r1,l2,l1" {
		t.Fatalf("persisted mixed strip order = %q, want r1,l2,l1", got)
	}

	if err := a.ReorderTabs([]string{"l1", "l2", "ghost"}); err == nil {
		t.Fatal("reorder accepted an unknown remote id")
	}
	a.mu.RLock()
	order = append([]string(nil), a.tabOrder...)
	a.mu.RUnlock()
	if len(order) != 2 || order[0] != "l2" || order[1] != "l1" {
		t.Fatalf("local order after rejected reorder = %v, want unchanged [l2 l1]", order)
	}
}

func TestReconcileTabStripOrderPreservesMixedOrderAndRepairsMembership(t *testing.T) {
	got := reconcileTabStripOrder(
		[]string{"remote-1", "gone", "local-2", "remote-1"},
		[]string{"local-1", "local-2"},
		[]string{"remote-1", "remote-2"},
	)
	if joined := strings.Join(got, ","); joined != "remote-1,local-2,local-1,remote-2" {
		t.Fatalf("reconciled strip order = %q", joined)
	}
}

func TestRemoteTabMetasMarksOnlySelectedTabActive(t *testing.T) {
	a := &App{
		remoteTabs: map[string]*remoteTab{
			"remote-1": {id: "remote-1", ref: RemoteTabRef{HostID: "box", Workspace: "~/one"}},
			"remote-2": {id: "remote-2", ref: RemoteTabRef{HostID: "box", Workspace: "~/two"}},
		},
		remoteTabLayout: remoteTabLayoutState{
			order:      []string{"remote-1", "remote-2"},
			activeID:   "remote-2",
			stripOrder: []string{"remote-1", "local-1", "remote-2"},
		},
	}
	metas, active, order := a.remoteTabMetas([]string{"local-1"})
	if active != "remote-2" || strings.Join(order, ",") != "remote-1,local-1,remote-2" {
		t.Fatalf("active/order = %q / %v", active, order)
	}
	for _, meta := range metas {
		if meta.Active != (meta.ID == "remote-2") {
			t.Fatalf("meta %s active = %v", meta.ID, meta.Active)
		}
	}
}

// TestSingleSurfaceTabsFileCollapsesRemote: workbench/creation layouts keep
// exactly one surface across local and remote tabs, preferring the active one.
func TestSingleSurfaceTabsFileCollapsesRemote(t *testing.T) {
	f := desktopTabsFile{
		Tabs:       []desktopTabEntry{{ID: "l1"}, {ID: "l2"}},
		RemoteTabs: []desktopRemoteTabEntry{{ID: "r1", HostID: "h", Workspace: "~/a"}, {ID: "r2", HostID: "h", Workspace: "~/b"}},
		ActiveTab:  "r1",
	}
	out := singleSurfaceTabsFile(f)
	if len(out.Tabs) != 0 || len(out.RemoteTabs) != 1 || out.RemoteTabs[0].ID != "r1" {
		t.Fatalf("single-surface collapse = %+v", out)
	}
	if out.ActiveTab != "r1" {
		t.Fatalf("collapsed ActiveTab = %q, want the remote r1", out.ActiveTab)
	}
}

func TestSingleSurfaceTabsFilePrefersActiveLocalOverRemote(t *testing.T) {
	f := desktopTabsFile{
		Tabs:       []desktopTabEntry{{ID: "l1"}, {ID: "l2"}},
		RemoteTabs: []desktopRemoteTabEntry{{ID: "r1", HostID: "h", Workspace: "~/a"}},
		ActiveTab:  "l2",
	}
	out := singleSurfaceTabsFile(f)
	if len(out.Tabs) != 1 || out.Tabs[0].ID != "l2" || len(out.RemoteTabs) != 0 || out.ActiveTab != "l2" {
		t.Fatalf("single-surface local collapse = %+v", out)
	}
}

// TestSuspendSkipsDisconnectedShells: host status transitions never flip a
// restored shell into a runtime state.
func TestSuspendSkipsDisconnectedShells(t *testing.T) {
	a := &App{}
	a.remoteTabMu.Lock()
	a.remoteTabs = map[string]*remoteTab{
		"shell": {id: "shell", ref: RemoteTabRef{HostID: "box", Workspace: "~/app"}, state: "disconnected"},
		"live":  {id: "live", ref: RemoteTabRef{HostID: "box", Workspace: "~/web"}, state: "ready"},
	}
	a.remoteTabMu.Unlock()
	a.suspendRemoteTabPumps("box", "reconnecting", "")
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	if a.remoteTabs["shell"].state != "disconnected" {
		t.Fatalf("shell state = %q, want disconnected", a.remoteTabs["shell"].state)
	}
	if a.remoteTabs["live"].state != "reconnecting" {
		t.Fatalf("live state = %q, want reconnecting", a.remoteTabs["live"].state)
	}
}

// TestTabsFileWithoutRemoteTabsKeepsLegacyShape: with no remote tabs open the
// persisted file carries no remote keys, so local-only usage stays
// byte-compatible with the pre-remote format.
func TestTabsFileWithoutRemoteTabsKeepsLegacyShape(t *testing.T) {
	seedBridgeTestHost(t, "box")
	a := &App{}
	seedLocalTab(a, "l1")
	a.mu.Lock()
	a.activeTabID = "l1"
	a.mu.Unlock()
	a.saveTabsFromRemote()
	data, err := os.ReadFile(filepath.Join(config.ReasonixHomeDir(), tabsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "remoteTabs") || strings.Contains(string(data), "remoteTabOrder") {
		t.Fatalf("tabs file mentions remote keys with none open:\n%s", data)
	}
}
