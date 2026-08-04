package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"reasonix/internal/config"
)

// TestRemoteWindowHelperProcess is the target of registry tests that need a
// real live child process: it is spawned via os.Args[0] with a marker env and
// blocks until the test kills it.
func TestRemoteWindowHelperProcess(t *testing.T) {
	if os.Getenv("REMOTE_WINDOW_HELPER_PROCESS") != "1" {
		return
	}
	time.Sleep(120 * time.Second)
}

func spawnRemoteWindowHelper(t *testing.T) *os.Process {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestRemoteWindowHelperProcess")
	cmd.Env = append(os.Environ(), "REMOTE_WINDOW_HELPER_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process
}

func waitRemoteWindowHelperExit(t *testing.T, proc *os.Process) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_, _ = proc.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("remote window helper process did not exit after kill")
	}
}

func TestRemoteWindowTicketRoundTripAndRemoval(t *testing.T) {
	launch := remoteWindowLaunch{
		URL:     "http://127.0.0.1:54321/?token=secret-token",
		Title:   "Reasonix [SSH: box]",
		HostKey: "host-key-digest",
	}
	ticket, err := writeRemoteWindowLaunch(launch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ticket, "secret-token") || filepath.Base(ticket) != ticket {
		t.Fatalf("ticket leaked URL data or path: %q", ticket)
	}
	path, err := remoteWindowTicketPath(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("ticket permissions = %o, want 600", got)
		}
	}
	got, err := consumeRemoteWindowLaunch(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if *got != launch {
		t.Fatalf("launch = %+v, want %+v", *got, launch)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ticket was not removed after consumption: %v", err)
	}
}

func TestRemoteWindowTicketRejectsUnsafeInputs(t *testing.T) {
	for _, raw := range []string{
		"https://127.0.0.1:5000/?token=x",
		"http://example.com:5000/?token=x",
		"file:///tmp/index.html",
		"javascript:alert(1)",
	} {
		if _, err := writeRemoteWindowLaunch(remoteWindowLaunch{URL: raw, HostKey: "k"}); err == nil {
			t.Fatalf("unsafe URL accepted: %q", raw)
		}
	}
	if _, err := writeRemoteWindowLaunch(remoteWindowLaunch{URL: "http://127.0.0.1:5000/"}); err == nil {
		t.Fatal("ticket without host identity accepted")
	}
	for _, ticket := range []string{"", "../.remote-window-x", "/tmp/.remote-window-x", "unrelated"} {
		if _, err := remoteWindowTicketPath(ticket); err == nil {
			t.Fatalf("unsafe ticket accepted: %q", ticket)
		}
	}
}

func TestConsumeRemoteWindowTicketRejectsBroadPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits through os.Stat")
	}
	dir := config.MemoryUserDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ticket := remoteWindowTicketPrefix + "insecure"
	path := filepath.Join(dir, ticket)
	if err := os.WriteFile(path, []byte(`{"url":"http://127.0.0.1:5000/","hostKey":"k"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if _, err := consumeRemoteWindowLaunch(ticket); err == nil {
		t.Fatal("ticket with broad permissions was accepted")
	}
}

func TestConsumeRemoteWindowTicketRejectsOversizedDescriptor(t *testing.T) {
	dir := config.MemoryUserDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ticket := remoteWindowTicketPrefix + "oversized"
	path := filepath.Join(dir, ticket)
	if err := os.WriteFile(path, make([]byte, remoteWindowTicketMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := consumeRemoteWindowLaunch(ticket); err == nil {
		t.Fatal("oversized ticket was accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected ticket was not removed: %v", err)
	}
}

func TestRemoteWindowNavigationJSEscapesURL(t *testing.T) {
	js, err := remoteWindowNavigationJS("http://127.0.0.1:5000/?token=x%22);alert(1)//")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(js, "window.location.replace(\"") || !strings.HasSuffix(js, "\");") {
		t.Fatalf("unexpected navigation JS: %q", js)
	}
	if strings.Contains(js, "\");alert") {
		t.Fatalf("URL escaped the JS string: %q", js)
	}
}

func TestRemoteWindowHostKeyDistinguishesHosts(t *testing.T) {
	a := remoteWindowHostKey("host-a")
	b := remoteWindowHostKey("host-b")
	if a == b {
		t.Fatal("distinct hosts share a window identity")
	}
	if remoteWindowHostKey("host-a") != a {
		t.Fatal("host key is not stable for the same host")
	}
	if strings.Contains(a, "host-a") || strings.Contains(b, "host-b") {
		t.Fatal("host key leaks the host label")
	}
	if remoteWindowInstanceID(a) == remoteWindowInstanceID(b) {
		t.Fatal("instance IDs collide across hosts")
	}
	if !strings.HasPrefix(remoteWindowInstanceID(a), remoteWindowInstancePrefix) {
		t.Fatalf("instance ID = %q, want %q prefix", remoteWindowInstanceID(a), remoteWindowInstancePrefix)
	}
}

func TestRemoteWindowRegistryGenerationProtectsNewerWindow(t *testing.T) {
	r := newRemoteWindowRegistry()
	key := "host-a"

	p1 := spawnRemoteWindowHelper(t)
	defer func() { _ = p1.Kill(); waitRemoteWindowHelperExit(t, p1) }()
	g1 := r.record(key, p1)
	if !r.has(key) {
		t.Fatal("first child not registered")
	}

	// The first child is replaced by a newer spawn for the same host. A stale
	// Wait from the old child must not clear the newer registration.
	p2 := spawnRemoteWindowHelper(t)
	defer func() { _ = p2.Kill(); waitRemoteWindowHelperExit(t, p2) }()
	g2 := r.record(key, p2)
	if g2 <= g1 {
		t.Fatalf("generation did not advance: %d then %d", g1, g2)
	}
	r.clearIf(key, g1, p1.Pid)
	if !r.has(key) {
		t.Fatal("stale Wait cleared the newer registration")
	}

	// The current generation's own Wait clears only its registration.
	r.clearIf(key, g2, p2.Pid)
	if r.has(key) {
		t.Fatal("registration not cleared by its own Wait")
	}
}

func TestRemoteWindowRegistryCloseTerminatesChild(t *testing.T) {
	r := newRemoteWindowRegistry()
	key := "host-b"
	p := spawnRemoteWindowHelper(t)
	r.record(key, p)
	r.close(key)
	waitRemoteWindowHelperExit(t, p)
	if r.has(key) {
		t.Fatal("closed child still registered")
	}
}

func TestRemoteWindowRegistryCloseAllTerminatesAll(t *testing.T) {
	r := newRemoteWindowRegistry()
	p1 := spawnRemoteWindowHelper(t)
	defer waitRemoteWindowHelperExit(t, p1)
	p2 := spawnRemoteWindowHelper(t)
	defer waitRemoteWindowHelperExit(t, p2)
	r.record("host-a", p1)
	r.record("host-b", p2)
	r.closeAll()
	waitRemoteWindowHelperExit(t, p1)
	waitRemoteWindowHelperExit(t, p2)
	if r.has("host-a") || r.has("host-b") {
		t.Fatal("closeAll left registrations behind")
	}
}

func TestRemoteWindowLifecycleSkipsPrimaryRuntime(t *testing.T) {
	a := NewApp()
	a.remoteWindowTicket = remoteWindowTicketPrefix + "test"
	a.startup(context.Background())
	if a.tabsRestored != nil {
		t.Fatal("remote window initialized local tab restore")
	}
	if a.heartbeat != nil || a.tray != nil || a.remoteRuntime != nil {
		t.Fatal("remote window initialized primary-process runtime")
	}
	if a.beforeClose(context.Background()) {
		t.Fatal("remote window close was intercepted")
	}
	a.shutdown(context.Background())
}

func TestRemoteWindowAssetMiddlewareDoesNotLoadPrimaryFrontend(t *testing.T) {
	a := &App{remoteWindowTicket: remoteWindowTicketPrefix + "shell"}
	nextCalled := false
	h := a.remoteWindowAssetMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if nextCalled {
		t.Fatal("remote shell loaded the primary asset handler")
	}
	if strings.Contains(rec.Body.String(), "<script") {
		t.Fatal("remote shell bootstrap unexpectedly contains frontend scripts")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestRemoteWindowAssetMiddlewarePassesThroughMainApp(t *testing.T) {
	a := &App{}
	nextCalled := false
	h := a.remoteWindowAssetMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !nextCalled {
		t.Fatal("main app shell intercepted by the remote window middleware")
	}
}

func TestRemoteWindowTitleSanitizesHostLabel(t *testing.T) {
	if got := remoteWindowTitle(" box\nprod "); got != "Reasonix [SSH: boxprod]" {
		t.Fatalf("title = %q", got)
	}
}

func TestServeURLWithToken(t *testing.T) {
	cases := []struct {
		localURL, token, want string
	}{
		{"http://127.0.0.1:54321/", "tok-1", "http://127.0.0.1:54321?token=tok-1"},
		{"http://127.0.0.1:54321", "tok-1", "http://127.0.0.1:54321?token=tok-1"},
		{"http://127.0.0.1:54321/?token=old", "tok-1", "http://127.0.0.1:54321/?token=old"},
		{"http://127.0.0.1:54321/", "", "http://127.0.0.1:54321/"},
	}
	for _, c := range cases {
		if got := serveURLWithToken(c.localURL, c.token); got != c.want {
			t.Fatalf("serveURLWithToken(%q, %q) = %q, want %q", c.localURL, c.token, got, c.want)
		}
	}
}

func TestOpenRemoteWorkspaceOpensWebWindow(t *testing.T) {
	fake := &fakeRemoteKernel{
		ensureView:  RemoteServerView{HostID: "box", Workspace: "/srv", State: "ready", LocalURL: "http://127.0.0.1:54321/"},
		ensureToken: "tok-123",
	}
	a := NewApp()
	a.remoteRuntime = fake
	calls := make(chan remoteWindowLaunch, 2)
	a.remoteWindowOpener = func(l remoteWindowLaunch) error {
		calls <- l
		return nil
	}
	if err := a.OpenRemoteWorkspace("box", "/srv"); err != nil {
		t.Fatal(err)
	}
	select {
	case l := <-calls:
		want := "http://127.0.0.1:54321?token=tok-123"
		if l.URL != want {
			t.Fatalf("window URL = %q, want %q", l.URL, want)
		}
		if !strings.Contains(l.Title, "box") {
			t.Fatalf("window title = %q, want host label", l.Title)
		}
		if l.HostKey != remoteWindowHostKey("box") {
			t.Fatalf("window host key = %q", l.HostKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no web window opened")
	}
	if got := a.RemoteLastWorkspace("box"); got != "/srv" {
		t.Fatalf("last workspace = %q, want /srv", got)
	}
}

func TestOpenRemoteWorkspaceFailureKeepsWindowUntouched(t *testing.T) {
	const hostID = "failing-box"
	fake := &fakeRemoteKernel{ensureErr: errors.New("serve failed")}
	a := NewApp()
	a.remoteRuntime = fake
	a.remoteWindowOpener = func(remoteWindowLaunch) error {
		t.Fatal("window opened on serve failure")
		return nil
	}
	if err := a.OpenRemoteWorkspace(hostID, "/srv"); err == nil {
		t.Fatal("expected serve failure to surface")
	}
	if got := a.RemoteLastWorkspace(hostID); got != "" {
		t.Fatalf("failed open recorded last workspace %q", got)
	}
}

// TestOpenRemoteWorkspaceWorkspaceSwitchRequiresServerSuccess covers the atomic
// switch contract: a new Serve + tunnel must be established before the window
// is re-pointed; a failed switch keeps the previous window and last workspace.
func TestOpenRemoteWorkspaceWorkspaceSwitchRequiresServerSuccess(t *testing.T) {
	fake := &fakeRemoteKernel{
		ensureView:  RemoteServerView{HostID: "box", Workspace: "/srv", State: "ready", LocalURL: "http://127.0.0.1:54321/"},
		ensureToken: "t1",
	}
	a := NewApp()
	a.remoteRuntime = fake
	calls := make(chan remoteWindowLaunch, 4)
	a.remoteWindowOpener = func(l remoteWindowLaunch) error {
		calls <- l
		return nil
	}
	if err := a.OpenRemoteWorkspace("box", "/srv"); err != nil {
		t.Fatal(err)
	}
	if l := <-calls; !strings.HasSuffix(l.URL, "token=t1") {
		t.Fatalf("first window URL = %q", l.URL)
	}

	// Successful switch to a new workspace re-points the window.
	fake.ensureView = RemoteServerView{HostID: "box", Workspace: "/srv2", State: "ready", LocalURL: "http://127.0.0.1:5555/"}
	fake.ensureToken = "t2"
	if err := a.OpenRemoteWorkspace("box", "/srv2"); err != nil {
		t.Fatal(err)
	}
	if l := <-calls; !strings.HasSuffix(l.URL, "token=t2") {
		t.Fatalf("switched window URL = %q", l.URL)
	}

	// Failed switch leaves the window and last workspace untouched.
	fake.ensureErr = errors.New("boom")
	if err := a.OpenRemoteWorkspace("box", "/srv3"); err == nil {
		t.Fatal("expected switch failure to surface")
	}
	select {
	case l := <-calls:
		t.Fatalf("window re-pointed on failed switch: %q", l.URL)
	case <-time.After(200 * time.Millisecond):
	}
	if got := a.RemoteLastWorkspace("box"); got != "/srv2" {
		t.Fatalf("last workspace after failed switch = %q, want /srv2", got)
	}
}

func TestRemoteWindowCloseOnTerminalDisconnectKeepsTransient(t *testing.T) {
	a := NewApp()
	key := remoteWindowHostKey("box")
	p := spawnRemoteWindowHelper(t)
	defer waitRemoteWindowHelperExit(t, p)
	a.remoteWindows.record(key, p)

	// Transient reconnect states keep the window.
	a.onStatus(RemoteConnectionStatusView{HostID: "box", State: "reconnecting"})
	a.onStatus(RemoteConnectionStatusView{HostID: "box", State: "degraded"})
	if !a.hasRemoteWindow("box") {
		t.Fatal("window closed during transient reconnect")
	}

	// A deterministic terminal failure closes it.
	a.onStatus(RemoteConnectionStatusView{HostID: "box", State: "stopped", Error: "auth failed"})
	waitRemoteWindowHelperExit(t, p)
	if a.hasRemoteWindow("box") {
		t.Fatal("window survived terminal disconnect")
	}
}

func TestRemoteWindowReconnectRepointsWindow(t *testing.T) {
	fake := &fakeRemoteKernel{
		ensureView:  RemoteServerView{HostID: "box", Workspace: "/srv", State: "ready", LocalURL: "http://127.0.0.1:9999/"},
		ensureToken: "tok-re",
	}
	a := NewApp()
	a.remoteRuntime = fake
	key := remoteWindowHostKey("box")
	p := spawnRemoteWindowHelper(t)
	defer func() { _ = p.Kill(); waitRemoteWindowHelperExit(t, p) }()
	a.remoteWindows.record(key, p)
	calls := make(chan remoteWindowLaunch, 2)
	a.remoteWindowOpener = func(l remoteWindowLaunch) error {
		calls <- l
		return nil
	}

	a.onStatus(RemoteConnectionStatusView{HostID: "box", State: "connected"})
	select {
	case l := <-calls:
		if l.URL != "http://127.0.0.1:9999?token=tok-re" {
			t.Fatalf("re-pointed URL = %q", l.URL)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("window not re-pointed after reconnect")
	}
}

func TestRemoteWindowDisconnectAndStopCloseWindow(t *testing.T) {
	fake := &fakeRemoteKernel{}
	a := NewApp()
	a.remoteRuntime = fake
	key := remoteWindowHostKey("box")
	p := spawnRemoteWindowHelper(t)
	defer waitRemoteWindowHelperExit(t, p)
	a.remoteWindows.record(key, p)

	if err := a.DisconnectRemoteHost("box"); err != nil {
		t.Fatal(err)
	}
	waitRemoteWindowHelperExit(t, p)
	if a.hasRemoteWindow("box") {
		t.Fatal("window survived explicit disconnect")
	}

	p2 := spawnRemoteWindowHelper(t)
	defer waitRemoteWindowHelperExit(t, p2)
	a.remoteWindows.record(key, p2)
	if err := a.StopRemoteServer("box"); err != nil {
		t.Fatal(err)
	}
	waitRemoteWindowHelperExit(t, p2)
	if a.hasRemoteWindow("box") {
		t.Fatal("window survived stop-server")
	}
}
