package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The remote-tab bridge exchanges its pre-shared token for an HttpOnly
// session cookie over the loopback tunnel. Subsequent API and SSE requests
// use that cookie, keeping the token out of request lines and access logs.

const remoteTabStreamOpenStability = 50 * time.Millisecond

// enterRemoteSession enters a fresh or named Serve session. Since /resume
// takes a path rather than a name, named sessions resolve through /sessions.
func enterRemoteSession(ctx context.Context, client *http.Client, base string, opts RemoteTabOpenOptions) error {
	name := strings.TrimSpace(opts.SessionName)
	if opts.NewSession || name == "" {
		return servePost(ctx, client, serveURL(base, "/new"), nil)
	}
	sessions, err := serveSessions(ctx, client, base)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.Name == name {
			body, err := json.Marshal(map[string]string{"path": s.Path})
			if err != nil {
				return err
			}
			return servePost(ctx, client, serveURL(base, "/resume"), body)
		}
	}
	return fmt.Errorf("remote session %q not found", name)
}

func remoteSessionTransitionBusy(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "status 409") || strings.Contains(message, "while a turn is running")
}

// attachRemoteTabServe starts the event pump before entering the session so
// /new or /resume frames are not missed. The caller's context owns the pump;
// handshake and session entry use a bounded child context.
func (a *App) attachRemoteTabServe(ctx context.Context, tabID, base, token string, opts RemoteTabOpenOptions) (bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, err := newServeHTTPClient(base)
	if err != nil {
		return false, err
	}
	if err := serveHandshake(callCtx, client, base, token); err != nil {
		log.Printf("[remote] attachRemoteTabServe: handshake FAILED tab=%s base=%q err=%v", tabID, base, err)
		return false, err
	}

	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	a.remoteTabMu.Unlock()
	if tab == nil {
		return false, fmt.Errorf("remote tab %q closed during bootstrap", tabID)
	}
	tab.sessionMu.Lock()
	defer tab.sessionMu.Unlock()

	a.remoteTabMu.Lock()
	if a.remoteTabs[tabID] != tab {
		a.remoteTabMu.Unlock()
		return false, fmt.Errorf("remote tab %q closed during bootstrap", tabID)
	}
	// Retire any pump installed by a concurrent reconnect so exactly one
	// generation owns the event stream.
	tab.gen++
	if tab.cancel != nil {
		tab.cancel()
	}
	tab.client = client
	tab.base = base
	tab.token = token
	gen := tab.gen
	pumpCtx, cancelPump := context.WithCancel(ctx)
	tab.cancel = cancelPump
	a.remoteTabMu.Unlock()

	opened := make(chan error, 1)
	a.goSafe("remoteTabPump", func() { a.remoteTabPump(pumpCtx, tabID, gen, opened) })
	select {
	case err = <-opened:
		if err != nil {
			a.retireRemoteTabGeneration(tabID, gen)
			return false, err
		}
	case <-callCtx.Done():
		a.retireRemoteTabGeneration(tabID, gen)
		return false, callCtx.Err()
	}
	err = enterRemoteSession(callCtx, client, base, opts)
	entered := err == nil
	if err != nil {
		// A busy serve refuses session transitions with 409 but retains its
		// usable current session. Keep the attach so pending work remains visible.
		if remoteSessionTransitionBusy(err) {
			log.Printf("[remote] attachRemoteTabServe: enterRemoteSession BUSY (attached to current session) tab=%s err=%v", tabID, err)
			entered = false
		} else {
			log.Printf("[remote] attachRemoteTabServe: enterRemoteSession FAILED tab=%s err=%v", tabID, err)
			a.retireRemoteTabGeneration(tabID, gen)
			return false, err
		}
	}
	if !a.waitRemoteTabStreamStable(callCtx, tabID, gen) {
		return false, fmt.Errorf("remote tab %q event stream closed during session attach", tabID)
	}
	// A 200 response is only the stream-open barrier. The stream can still die
	// while /new or /resume is in flight; publish readiness only if its pump has
	// not already moved this same generation into reconnecting/error.
	if !a.markRemoteTabAttached(tabID, gen) {
		return false, fmt.Errorf("remote tab %q event stream closed during session attach", tabID)
	}
	return entered, nil
}

func (a *App) waitRemoteTabStreamStable(ctx context.Context, tabID string, gen uint64) bool {
	timer := time.NewTimer(remoteTabStreamOpenStability)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return a.remoteTabGenerationCurrent(tabID, gen)
	}
}

func (a *App) markRemoteTabAttached(tabID string, gen uint64) bool {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen || tab.state != "connecting" {
		return false
	}
	tab.attachedGen = gen
	return true
}

func (a *App) publishRemoteTabAttachedReady(tabID string, gen uint64) bool {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen || tab.attachedGen != gen || tab.state != "connecting" {
		a.remoteTabMu.Unlock()
		return false
	}
	tab.attachedGen = 0
	tab.state = "ready"
	tab.err = ""
	a.remoteTabMu.Unlock()
	a.emitRemoteEvent(fmt.Sprintf("remote-tab:%s:state", tabID), RemoteTabStateView{State: "ready"})
	return true
}

func (a *App) remoteTabGenerationCurrent(tabID string, gen uint64) bool {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	tab := a.remoteTabs[tabID]
	return tab != nil && tab.gen == gen
}

func (a *App) retireRemoteTabGeneration(tabID string, gen uint64) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen {
		a.remoteTabMu.Unlock()
		return
	}
	cancel := tab.cancel
	tab.gen++
	tab.attachedGen = 0
	tab.cancel = nil
	tab.client = nil
	tab.base = ""
	tab.token = ""
	a.remoteTabMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// reconnectRemoteTabGeneration retires a dead pump and atomically parks its
// tab in reconnecting. The bool reports whether this pump should start the
// retry loop; a pump opened by an existing retry loop leaves retries to its
// caller so two loops cannot race each other.
func (a *App) reconnectRemoteTabGeneration(tabID string, gen uint64) bool {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen {
		a.remoteTabMu.Unlock()
		return false
	}
	startRetry := tab.state != "reconnecting"
	cancel := tab.cancel
	tab.gen++
	tab.attachedGen = 0
	tab.cancel = nil
	tab.client = nil
	tab.base = ""
	tab.token = ""
	tab.state = "reconnecting"
	tab.err = ""
	a.remoteTabMu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.emitRemoteEvent(fmt.Sprintf("remote-tab:%s:state", tabID), RemoteTabStateView{State: "reconnecting"})
	return startRetry
}

func (a *App) emitRemoteTabStateForGeneration(tabID string, gen uint64, state, errMsg string) bool {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen {
		a.remoteTabMu.Unlock()
		return false
	}
	tab.state = state
	tab.err = errMsg
	a.remoteTabMu.Unlock()
	a.emitRemoteEvent(fmt.Sprintf("remote-tab:%s:state", tabID), RemoteTabStateView{State: state, Error: errMsg})
	return true
}

func (a *App) transitionRemoteTabState(tabID string, gen uint64, from, state, errMsg string) bool {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen || tab.state != from {
		a.remoteTabMu.Unlock()
		return false
	}
	tab.state = state
	tab.err = errMsg
	a.remoteTabMu.Unlock()
	a.emitRemoteEvent(fmt.Sprintf("remote-tab:%s:state", tabID), RemoteTabStateView{State: state, Error: errMsg})
	return true
}

// remoteTabPump forwards Serve events for one tab generation. Cancellation,
// stream death, or a generation mismatch retires the pump.
func (a *App) remoteTabPump(ctx context.Context, tabID string, gen uint64, opened chan<- error) {
	signalOpened := func(err error) {
		if opened == nil {
			return
		}
		select {
		case opened <- err:
		default:
		}
		opened = nil
	}
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	var client *http.Client
	var base string
	if tab != nil && tab.gen == gen {
		client, base = tab.client, tab.base
	}
	a.remoteTabMu.Unlock()
	if client == nil || base == "" {
		if opened != nil {
			opened <- fmt.Errorf("remote tab %q event stream was retired before opening", tabID)
		}
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serveURL(base, "/events"), nil)
	if err != nil {
		signalOpened(err)
		a.emitRemoteTabStateForGeneration(tabID, gen, "error", err.Error())
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		signalOpened(err)
		if ctx.Err() == nil {
			log.Printf("[remote] remoteTabPump: /events DO-FAILED tab=%s err=%v", tabID, err)
			a.emitRemoteTabStateForGeneration(tabID, gen, "error", err.Error())
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("serve /events: status %d", resp.StatusCode)
		signalOpened(err)
		log.Printf("[remote] remoteTabPump: /events BAD-STATUS tab=%s status=%d", tabID, resp.StatusCode)
		a.emitRemoteTabStateForGeneration(tabID, gen, "error", err.Error())
		return
	}
	signalOpened(nil)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), serveEventMaxBytes)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue // ": ping" keepalives and other SSE fields
		}
		frame := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if frame == "" {
			continue
		}
		if !a.remoteTabGenerationCurrent(tabID, gen) {
			return
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		kind := "?"
		if json.Unmarshal([]byte(frame), &probe) == nil && probe.Kind != "" {
			kind = probe.Kind
		}
		switch kind {
		case "approval_request", "ask_request":
			a.cacheRemotePendingEvent(tabID, gen, kind, json.RawMessage(frame))
		case "turn_done":
			a.completeRemoteTabTurn(tabID, gen)
		}
		a.emitRemoteEvent(fmt.Sprintf("remote-tab:%s:event", tabID), json.RawMessage(frame))
		if kind == "turn_done" {
			// The serve generates the session title from the finished
			// conversation; pick it up shortly after the turn settles.
			a.goSafe("remoteTabTitle", func() {
				time.Sleep(1500 * time.Millisecond)
				a.refreshRemoteTabTitle(tabID)
			})
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[remote] remoteTabPump: READ-EXIT tab=%s gen=%d err=%v ctxErr=%v", tabID, gen, err, ctx.Err())
	}
	// Only the current generation reacts to an unexpected stream death.
	// Reattach now; the host status hook also retries on connection recovery.
	if ctx.Err() == nil {
		if startRetry := a.reconnectRemoteTabGeneration(tabID, gen); startRetry {
			a.goSafe("remoteTabReattach", func() { a.reattachRemoteTab(tabID) })
		}
	}
}

func (a *App) completeRemoteTabTurn(tabID string, gen uint64) {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	if tab := a.remoteTabs[tabID]; tab != nil && tab.gen == gen {
		tab.pendingEvents = nil
		// A completed turn makes the fresh session non-blank even when the
		// best-effort /sessions title lookup fails. New Topic must never reuse
		// a conversation that already has a completed turn.
		tab.session.reset = false
	}
}

func (a *App) cacheRemotePendingEvent(tabID string, gen uint64, kind string, frame json.RawMessage) {
	var probe struct {
		Approval *struct {
			ID string `json:"id"`
		} `json:"approval"`
		Ask *struct {
			ID string `json:"id"`
		} `json:"ask"`
	}
	_ = json.Unmarshal(frame, &probe)
	id := ""
	if probe.Approval != nil {
		id = probe.Approval.ID
	} else if probe.Ask != nil {
		id = probe.Ask.ID
	}
	key := kind + ":" + strings.TrimSpace(id)
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen {
		return
	}
	if tab.pendingEvents == nil {
		tab.pendingEvents = make(map[string]json.RawMessage)
	}
	tab.pendingEvents[key] = append(json.RawMessage(nil), frame...)
}

func (a *App) clearRemotePendingEvent(tabID, kind, callID string) {
	a.remoteTabMu.Lock()
	if tab := a.remoteTabs[tabID]; tab != nil {
		delete(tab.pendingEvents, kind+":"+strings.TrimSpace(callID))
	}
	a.remoteTabMu.Unlock()
}

// serveGet fetches a JSON member of the tab snapshot, returning the raw
// payload for verbatim passthrough.
func serveGet(ctx context.Context, client *http.Client, url string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, serveSnapshotMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > serveSnapshotMaxBytes {
		return nil, fmt.Errorf("%s: response exceeds %d bytes", url, serveSnapshotMaxBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return json.RawMessage(data), nil
}

// commandContext bounds one proxied command. Boot context when available;
// the timeout keeps a wedged tunnel from hanging the binding call.
func commandContext(a *App) (context.Context, context.CancelFunc) {
	ctx := a.bootContext()
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, 15*time.Second)
}

// remoteTabCommandClient resolves a tabID to its live serve client. A tab
// that has not finished bootstrap, is reconnecting, or has failed is an
// error, not a silent no-op.
func (a *App) remoteTabCommandClient(tabID string) (*http.Client, string, error) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	var client *http.Client
	var base string
	usable := tab != nil && tab.client != nil && tab.state == "ready"
	if usable {
		client, base = tab.client, tab.base
	}
	a.remoteTabMu.Unlock()
	if !usable {
		log.Printf("[remote] remoteTabCommandClient: REFUSED tab=%q (tab=%v client=%v)", tabID, tab != nil, tab != nil && tab.client != nil)
		return nil, "", fmt.Errorf("remote tab %q is not connected", tabID)
	}
	return client, base, nil
}

func (a *App) isRemoteTab(tabID string) bool {
	if strings.TrimSpace(tabID) == "" {
		return false
	}
	a.remoteTabMu.Lock()
	_, ok := a.remoteTabs[tabID]
	a.remoteTabMu.Unlock()
	return ok
}

// remoteTabRefFor returns the host+workspace ref when tabID belongs to a
// remote tab; view builders use it to mark remote-shaped metas.
func (a *App) remoteTabRefFor(tabID string) (RemoteTabRef, bool) {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	if tab := a.remoteTabs[tabID]; tab != nil {
		return tab.ref, true
	}
	return RemoteTabRef{}, false
}

func (a *App) remoteTabCurrentModel(tabID string) (string, bool) {
	if !a.isRemoteTab(tabID) {
		return "", false
	}
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	cur := ""
	if tab != nil {
		cur = tab.model
	}
	a.remoteTabMu.Unlock()
	return cur, true
}

func (a *App) SubmitRemoteTab(tabID, text string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"input": text})
	started := time.Now()
	err = servePost(ctx, client, serveURL(base, "/submit"), body)
	if err != nil {
		log.Printf("[remote] submit failed tab=%s dur=%s err=%v", tabID, time.Since(started).Round(time.Millisecond), err)
	}
	return err
}

func (a *App) CancelRemoteTab(tabID string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	return servePost(ctx, client, serveURL(base, "/cancel"), nil)
}

// ApproveRemoteTab answers a tool-approval request. Serve takes
// {id, allow, session, persist}; the frontend's decision string maps to the
// allow bool ("allow" ⇒ true), session/persist stay false.
func (a *App) ApproveRemoteTab(tabID, callID, decision string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"id": callID, "allow": strings.EqualFold(strings.TrimSpace(decision), "allow")})
	if err := servePost(ctx, client, serveURL(base, "/approve"), body); err != nil {
		return err
	}
	a.clearRemotePendingEvent(tabID, "approval_request", callID)
	return nil
}

type RemoteAskAnswer struct {
	QuestionID string   `json:"QuestionID"`
	Selected   []string `json:"Selected"`
}

// AnswerRemoteTab preserves the batch ask id at the top level and sends every
// question's own id/selections in the Serve AskAnswer wire shape.
func (a *App) AnswerRemoteTab(tabID, callID string, answers []RemoteAskAnswer) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]any{
		"id":      callID,
		"answers": answers,
	})
	if err := servePost(ctx, client, serveURL(base, "/answer"), body); err != nil {
		return err
	}
	a.clearRemotePendingEvent(tabID, "ask_request", callID)
	return nil
}

// RewindRemoteTab rewinds to a checkpoint. Serve identifies checkpoints by
// TURN index and takes {turn, scope}; the checkpointID string is that turn.
func (a *App) RewindRemoteTab(tabID, checkpointID, scope string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	turn, convErr := strconv.Atoi(strings.TrimSpace(checkpointID))
	if convErr != nil {
		return fmt.Errorf("invalid checkpoint id %q: want the turn index", checkpointID)
	}
	scope = strings.TrimSpace(scope)
	switch scope {
	case "code", "conversation", "both":
	default:
		return fmt.Errorf("invalid rewind scope %q", scope)
	}
	body, _ := json.Marshal(map[string]any{"turn": turn, "scope": scope})
	return servePost(ctx, client, serveURL(base, "/rewind"), body)
}

func (a *App) SetRemoteTabToolApprovalMode(tabID, mode string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"mode": mode})
	return servePost(ctx, client, serveURL(base, "/tool-approval-mode"), body)
}

func (a *App) SetRemoteTabGoal(tabID, goal string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"goal": goal})
	return servePost(ctx, client, serveURL(base, "/goal"), body)
}

// RemoteTabSnapshot mirrors the frontend shape: raw serve payloads passed
// through verbatim so the surface decides how to consume them.
type RemoteTabSnapshot struct {
	History       json.RawMessage   `json:"history"`
	Context       json.RawMessage   `json:"context,omitempty"`
	Todos         json.RawMessage   `json:"todos,omitempty"`
	Checkpoints   json.RawMessage   `json:"checkpoints,omitempty"`
	Models        json.RawMessage   `json:"models,omitempty"`
	Status        json.RawMessage   `json:"status,omitempty"`
	PendingEvents []json.RawMessage `json:"pendingEvents,omitempty"`
}

// RemoteTabSnapshot merges the serve's GET members in parallel. Only
// /history is required; the optional members degrade to absent on failure.
func (a *App) RemoteTabSnapshot(tabID string) (RemoteTabSnapshot, error) {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return RemoteTabSnapshot{}, err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	var snap RemoteTabSnapshot
	var wg sync.WaitGroup
	var mu sync.Mutex
	var historyErr error
	for path, dst := range map[string]*json.RawMessage{
		"/history":     &snap.History,
		"/context":     &snap.Context,
		"/todos":       &snap.Todos,
		"/checkpoints": &snap.Checkpoints,
		"/models":      &snap.Models,
		"/status":      &snap.Status,
	} {
		wg.Add(1)
		go func(path string, dst *json.RawMessage) {
			defer wg.Done()
			data, err := serveGet(ctx, client, serveURL(base, path))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if path == "/history" && historyErr == nil {
					historyErr = err
				}
				return
			}
			*dst = data
		}(path, dst)
	}
	wg.Wait()
	if historyErr != nil {
		return RemoteTabSnapshot{}, historyErr
	}
	if len(snap.History) == 0 {
		return RemoteTabSnapshot{}, fmt.Errorf("remote tab %q: empty history", tabID)
	}
	a.remoteTabMu.Lock()
	if tab := a.remoteTabs[tabID]; tab != nil {
		keys := make([]string, 0, len(tab.pendingEvents))
		for key := range tab.pendingEvents {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			snap.PendingEvents = append(snap.PendingEvents, append(json.RawMessage(nil), tab.pendingEvents[key]...))
		}
	}
	a.remoteTabMu.Unlock()
	return snap, nil
}

// RemoteTabStatus is the small status-only binding used by watchdog and close
// policy polling. It deliberately avoids transferring full history.
func (a *App) RemoteTabStatus(tabID string) (json.RawMessage, error) {
	return a.remoteTabGet(tabID, "/status")
}

// listTabsWithRemote merges the remote strip entries into a local tab list.
// A highlighted remote tab deactivates every local entry so the strip shows
// exactly one active tab.
func (a *App) listTabsWithRemote(local []TabMeta) []TabMeta {
	localIDs := make([]string, 0, len(local))
	for _, meta := range local {
		localIDs = append(localIDs, meta.ID)
	}
	remote, remoteActive, stripOrder := a.remoteTabMetas(localIDs)
	if remoteActive != "" {
		for i := range local {
			local[i].Active = false
		}
	}
	if len(remote) == 0 {
		return enrichTabMetas(local)
	}
	all := append(enrichTabMetas(local), remote...)
	byID := make(map[string]TabMeta, len(all))
	for _, meta := range all {
		byID[meta.ID] = meta
	}
	out := make([]TabMeta, 0, len(all))
	for _, id := range stripOrder {
		if meta, ok := byID[id]; ok {
			out = append(out, meta)
		}
	}
	return out
}
