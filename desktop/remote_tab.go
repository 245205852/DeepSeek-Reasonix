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

	a.goSafe("remoteTabPump", func() { a.remoteTabPump(pumpCtx, tabID, gen) })
	err = enterRemoteSession(callCtx, client, base, opts)
	if err != nil {
		// A busy serve refuses session transitions with 409 but retains its
		// usable current session. Keep the attach so pending work remains visible.
		if strings.Contains(err.Error(), "status 409") || strings.Contains(err.Error(), "while a turn is running") {
			log.Printf("[remote] attachRemoteTabServe: enterRemoteSession BUSY (attached to current session) tab=%s err=%v", tabID, err)
			return false, nil
		}
		log.Printf("[remote] attachRemoteTabServe: enterRemoteSession FAILED tab=%s err=%v", tabID, err)
		a.retireRemoteTabGeneration(tabID, gen)
		return false, err
	}
	return true, nil
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
	tab.cancel = nil
	tab.client = nil
	tab.base = ""
	tab.token = ""
	a.remoteTabMu.Unlock()
	if cancel != nil {
		cancel()
	}
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

// remoteTabPump forwards Serve events for one tab generation. Cancellation,
// stream death, or a generation mismatch retires the pump.
func (a *App) remoteTabPump(ctx context.Context, tabID string, gen uint64) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	var client *http.Client
	var base string
	if tab != nil && tab.gen == gen {
		client, base = tab.client, tab.base
	}
	a.remoteTabMu.Unlock()
	if client == nil || base == "" {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serveURL(base, "/events"), nil)
	if err != nil {
		a.emitRemoteTabStateForGeneration(tabID, gen, "error", err.Error())
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[remote] remoteTabPump: /events DO-FAILED tab=%s err=%v", tabID, err)
			a.emitRemoteTabStateForGeneration(tabID, gen, "error", err.Error())
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[remote] remoteTabPump: /events BAD-STATUS tab=%s status=%d", tabID, resp.StatusCode)
		a.emitRemoteTabStateForGeneration(tabID, gen, "error", fmt.Sprintf("serve /events: status %d", resp.StatusCode))
		return
	}
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
	if ctx.Err() == nil && a.emitRemoteTabStateForGeneration(tabID, gen, "reconnecting", "") {
		a.goSafe("remoteTabReattach", func() { a.reattachRemoteTab(tabID) })
	}
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
	usable := tab != nil && tab.client != nil && tab.state != "reconnecting" && tab.state != "error"
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
	return servePost(ctx, client, serveURL(base, "/approve"), body)
}

// AnswerRemoteTab answers an ask_request. Serve decodes event.AskAnswer
// (no json tags ⇒ fields marshal as QuestionID/Selected); callID doubles as
// the question id for the single-answer desktop shape.
func (a *App) AnswerRemoteTab(tabID, callID, answer string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]any{
		"id":      callID,
		"answers": []map[string]any{{"QuestionID": callID, "Selected": []string{answer}}},
	})
	return servePost(ctx, client, serveURL(base, "/answer"), body)
}

// RewindRemoteTab rewinds to a checkpoint. Serve identifies checkpoints by
// TURN index and takes {turn, scope}; the checkpointID string is that turn.
func (a *App) RewindRemoteTab(tabID, checkpointID string) error {
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
	body, _ := json.Marshal(map[string]any{"turn": turn, "scope": "both"})
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
	History     json.RawMessage `json:"history"`
	Context     json.RawMessage `json:"context,omitempty"`
	Todos       json.RawMessage `json:"todos,omitempty"`
	Checkpoints json.RawMessage `json:"checkpoints,omitempty"`
	Models      json.RawMessage `json:"models,omitempty"`
	Status      json.RawMessage `json:"status,omitempty"`
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
	return snap, nil
}

// remoteTabMetas returns chrome metas for every open remote tab (in strip
// order) plus the currently highlighted remote tab id ("" when a local tab is
// active).
func reconcileTabStripOrder(preferred, localIDs, remoteIDs []string) []string {
	valid := make(map[string]bool, len(localIDs)+len(remoteIDs))
	for _, id := range localIDs {
		valid[id] = true
	}
	for _, id := range remoteIDs {
		valid[id] = true
	}
	seen := make(map[string]bool, len(valid))
	out := make([]string, 0, len(valid))
	appendID := func(id string) {
		if valid[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range preferred {
		appendID(id)
	}
	for _, id := range localIDs {
		appendID(id)
	}
	for _, id := range remoteIDs {
		appendID(id)
	}
	return out
}

func (a *App) remoteTabMetas(localIDs []string) ([]TabMeta, string, []string) {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	ids := a.orderedRemoteTabIDsLocked()
	metas := make([]TabMeta, 0, len(ids))
	for _, id := range ids {
		if tab := a.remoteTabs[id]; tab != nil {
			meta := remoteTabMetaLocked(tab)
			meta.Active = id == a.remoteTabLayout.activeID
			metas = append(metas, meta)
		}
	}
	a.remoteTabLayout.stripOrder = reconcileTabStripOrder(a.remoteTabLayout.stripOrder, localIDs, ids)
	return metas, a.remoteTabLayout.activeID, append([]string(nil), a.remoteTabLayout.stripOrder...)
}

// orderedRemoteTabIDsLocked returns the remote strip order with self-repair:
// registry keys missing from the order append in sorted order (mirrors
// orderedTabIDsLocked for the local side). Caller holds remoteTabMu.
func (a *App) orderedRemoteTabIDsLocked() []string {
	seen := make(map[string]bool, len(a.remoteTabLayout.order))
	out := make([]string, 0, len(a.remoteTabs))
	for _, id := range a.remoteTabLayout.order {
		if a.remoteTabs[id] != nil && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	var missing []string
	for id := range a.remoteTabs {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return append(out, missing...)
}

// remoteTabsFileEntries snapshots the persisted remote tab section (entries
// plus strip order plus the active remote id). Called from the tab-file write
// path — lock order tabsSaveMu → remoteTabMu.
func (a *App) remoteTabsFileEntries(localIDs []string) ([]desktopRemoteTabEntry, []string, []string, string) {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	ids := a.orderedRemoteTabIDsLocked()
	entries := make([]desktopRemoteTabEntry, 0, len(ids))
	for _, id := range ids {
		tab := a.remoteTabs[id]
		if tab == nil {
			continue
		}
		entries = append(entries, desktopRemoteTabEntry{
			ID:         tab.id,
			HostID:     tab.ref.HostID,
			Workspace:  tab.ref.Workspace,
			TopicTitle: tab.topicTitle,
			Model:      tab.model,
		})
	}
	order := append([]string(nil), ids...)
	if len(order) == 0 {
		order = nil
	}
	stripOrder := reconcileTabStripOrder(a.remoteTabLayout.stripOrder, localIDs, ids)
	if len(entries) == 0 {
		stripOrder = nil
	}
	a.remoteTabLayout.stripOrder = append([]string(nil), stripOrder...)
	return entries, order, stripOrder, a.remoteTabLayout.activeID
}

// CloseRemoteTab tears down one remote tab: the SSE pump stops and the
// registry entry goes away. The remote serve and the SSH connection stay
// untouched — other tabs on the same host keep running.
func (a *App) CloseRemoteTab(tabID string) error {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	closingIndex := -1
	for i, id := range a.remoteTabLayout.stripOrder {
		if id == tabID {
			closingIndex = i
			break
		}
	}
	delete(a.remoteTabs, tabID)
	a.remoteTabLayout.order = removeRemoteTabOrderID(a.remoteTabLayout.order, tabID)
	if a.remoteTabLayout.activeID == tabID {
		a.remoteTabLayout.activeID = ""
		remaining := removeRemoteTabOrderID(append([]string(nil), a.remoteTabLayout.stripOrder...), tabID)
		if len(remaining) > 0 && closingIndex >= 0 {
			nextIndex := closingIndex
			if nextIndex >= len(remaining) {
				nextIndex = len(remaining) - 1
			}
			if a.remoteTabs[remaining[nextIndex]] != nil {
				a.remoteTabLayout.activeID = remaining[nextIndex]
			}
		}
	}
	var cancel context.CancelFunc
	if tab != nil {
		cancel = tab.cancel
	}
	a.remoteTabMu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.saveTabsFromRemote()
	return nil
}

// remoteTabsHostStatus reacts to SSH transitions for every open tab on the
// host: losing the tunnel suspends the pumps, a regained connection
// re-attaches each tab to the still-running remote serve, and a terminal
// failure parks the tabs in error.
func (a *App) remoteTabsHostStatus(hostID, state, errText string) {
	switch state {
	case "connecting", "reconnecting":
		a.suspendRemoteTabPumps(hostID, "reconnecting", "")
	case "connected":
		a.resumeRemoteTabs(hostID)
	case "stopped":
		a.suspendRemoteTabPumps(hostID, "error", errText)
	}
}

func (a *App) suspendRemoteTabPumps(hostID, state, errText string) {
	a.remoteTabMu.Lock()
	for _, tab := range a.remoteTabs {
		if tab.ref.HostID != hostID || tab.state == "disconnected" || (tab.state == "connecting" && tab.client == nil) {
			// A restored shell was never connected this run: host status
			// transitions must not flip it into a runtime state. The same is
			// true for a first bootstrap that is still waiting for that host.
			continue
		}
		tab.gen++
		if tab.cancel != nil {
			tab.cancel()
			tab.cancel = nil
		}
		tab.state = state
		tab.err = errText
	}
	a.remoteTabMu.Unlock()
}

// resumeRemoteTabs re-attaches every suspended tab of a reconnected host.
// The remote serve kept running through the SSH drop, so re-attachment only
// rebuilds the tunnel client and the event pump; the serve still holds the
// active session, so no session re-entry is needed.
func (a *App) resumeRemoteTabs(hostID string) {
	a.remoteTabMu.Lock()
	tabIDs := make([]string, 0, 2)
	for id, tab := range a.remoteTabs {
		if tab.ref.HostID == hostID && tab.state == "reconnecting" {
			tabIDs = append(tabIDs, id)
		}
	}
	a.remoteTabMu.Unlock()
	for _, tabID := range tabIDs {
		a.goSafe("remoteTabReattach", func() { a.reattachRemoteTab(tabID) })
	}
}

// reattachRemoteTab rebuilds one tab's serve client and pump after the
// host connection came back. Any failure leaves the tab in reconnecting —
// the next connected transition retries.
func (a *App) reattachRemoteTab(tabID string) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.state != "reconnecting" {
		a.remoteTabMu.Unlock()
		return
	}
	hostID, workspace := tab.ref.HostID, tab.ref.Workspace
	a.remoteTabMu.Unlock()

	rt, err := a.remoteRT()
	if err != nil {
		return
	}
	ctx := a.bootContext()
	if ctx == nil {
		ctx = context.Background()
	}
	view, token, err := rt.EnsureServer(ctx, hostID, workspace)
	if err != nil || view.State != "ready" || view.LocalURL == "" {
		log.Printf("[remote] reattachRemoteTab: EnsureServer NOT-READY tab=%s err=%v state=%s localURL=%q", tabID, err, view.State, view.LocalURL)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, clientErr := newServeHTTPClient(view.LocalURL)
	if clientErr != nil {
		return
	}
	if err := serveHandshake(callCtx, client, view.LocalURL, token); err != nil {
		log.Printf("[remote] reattachRemoteTab: handshake FAILED tab=%s base=%q err=%v", tabID, view.LocalURL, err)
		return
	}

	a.remoteTabMu.Lock()
	if cur := a.remoteTabs[tabID]; cur != tab || tab.state != "reconnecting" {
		a.remoteTabMu.Unlock()
		return
	}
	tab.gen++
	if tab.cancel != nil {
		tab.cancel()
	}
	tab.client = client
	tab.base = view.LocalURL
	tab.token = token
	gen := tab.gen
	pumpCtx, cancelPump := context.WithCancel(ctx)
	tab.cancel = cancelPump
	a.remoteTabMu.Unlock()

	a.goSafe("remoteTabPump", func() { a.remoteTabPump(pumpCtx, tabID, gen) })
	a.emitRemoteTabState(tabID, "ready", "")
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
