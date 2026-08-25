package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRemoteTabSnapshotReplaysAndClearsPendingPrompt(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})
	a.remoteTabMu.Lock()
	gen := a.remoteTabs[meta.ID].gen
	a.remoteTabMu.Unlock()
	a.cacheRemotePendingEvent(meta.ID, gen, "approval_request", json.RawMessage(`{"kind":"approval_request","approval":{"id":"approval-1","tool":"bash"}}`))
	snap, err := a.RemoteTabSnapshot(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.PendingEvents) != 1 || !strings.Contains(string(snap.PendingEvents[0]), "approval-1") {
		t.Fatalf("pending replay = %s", snap.PendingEvents)
	}
	if err := a.ApproveRemoteTab(meta.ID, "approval-1", "deny"); err != nil {
		t.Fatal(err)
	}
	snap, err = a.RemoteTabSnapshot(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.PendingEvents) != 0 {
		t.Fatalf("resolved prompt was still replayed: %s", snap.PendingEvents)
	}
}

func TestRemoteTabDoesNotPublishReadyWithoutEventStream(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	fs.mu.Lock()
	fs.eventsStatus = http.StatusServiceUnavailable
	fs.mu.Unlock()
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "error")
	time.Sleep(50 * time.Millisecond)
	a.remoteTabMu.Lock()
	state := a.remoteTabs[meta.ID].state
	a.remoteTabMu.Unlock()
	if state == "ready" {
		t.Fatal("tab published ready after /events failed")
	}
}

func TestRemoteTabDoesNotPublishReadyWhenEventStreamClosesDuringAttach(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	fs.mu.Lock()
	fs.eventsCloseEarly = true
	fs.enterDelay = 100 * time.Millisecond
	fs.mu.Unlock()
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	log := &eventLog{}
	a := &App{remoteRuntime: kernel, remoteEventHook: log.add}
	cleanupRemoteTabPumps(t, a)
	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "serve_down")
	for _, event := range log.recorded() {
		if strings.HasPrefix(event, "remote-tab:"+meta.ID+":state ") && strings.Contains(event, `"state":"ready"`) {
			t.Fatalf("closed event stream published ready: %v", log.recorded())
		}
	}
}

func TestRemoteTabReviveAppliesRequestedNamedSession(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{{Name: "saved", Path: "/saved.jsonl", Title: "Saved"}})
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[meta.ID]
	if tab.cancel != nil {
		tab.cancel()
	}
	tab.gen++
	tab.cancel, tab.client, tab.base, tab.token = nil, nil, "", ""
	tab.state = "disconnected"
	tab.session = remoteTabSessionState{newSession: true}
	a.remoteTabMu.Unlock()
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{SessionName: "saved"}); err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")
	_, resumed, _ := fs.snapshot()
	if resumed != "/saved.jsonl" {
		t.Fatalf("revived shell resumed %q, want the selected session", resumed)
	}
}

func TestRemoteTabServeDownCanRetry(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "error", Error: "temporary"}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "serve_down")
	kernel.ensureView = RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true}); err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")
}

func TestRemoteTabServeDownRetryPreservesNamedSession(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{{Name: "saved", Path: "/saved.jsonl", Title: "Saved"}})
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "saved"})
	a.parkRemoteTabsForServer("box", "~/app", "serve_down", "stopped")
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")
	_, resumed, _ := fs.snapshot()
	if resumed != "/saved.jsonl" {
		t.Fatalf("retry resumed %q, want the parked named session", resumed)
	}
}

func TestRemoteResumeBusyKeepsCurrentSessionReady(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{{Name: "saved", Path: "/saved.jsonl", Title: "Saved"}})
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})
	fs.mu.Lock()
	fs.failEnter = "cannot resume while a turn is running"
	fs.mu.Unlock()
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{SessionName: "saved"}); err != nil {
		t.Fatal(err)
	}
	a.remoteTabMu.Lock()
	state, message := a.remoteTabs[meta.ID].state, a.remoteTabs[meta.ID].err
	a.remoteTabMu.Unlock()
	if state != "ready" || !strings.Contains(message, "Finish the current turn") {
		t.Fatalf("busy resume state/error = %q/%q, want ready non-terminal notice", state, message)
	}
}

func TestRemoteStopAndCloseCancelsBeforeRemovingTab(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	fs.mu.Lock()
	fs.statusPayload = `{"running":true,"pendingPrompt":false,"backgroundJobs":1,"cancellable":true}`
	fs.statusAfterCancel = `{"running":false,"pendingPrompt":false,"backgroundJobs":0,"cancellable":false}`
	fs.mu.Unlock()
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})
	work := a.ActiveWorkForTab(meta.ID)
	if !work.Running || !work.Cancellable {
		t.Fatalf("remote active work = %+v", work)
	}
	if err := a.CloseTabWithPolicy(meta.ID, "stop_and_close"); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(fs.recorded(), func(call string) bool { return strings.HasPrefix(call, "POST /cancel") }) {
		t.Fatalf("stop-and-close did not cancel remote work: %v", fs.recorded())
	}
	a.remoteTabMu.Lock()
	_, present := a.remoteTabs[meta.ID]
	a.remoteTabMu.Unlock()
	if present {
		t.Fatal("remote tab remained after work became idle")
	}
}
