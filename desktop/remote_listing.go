package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// Listing-only slice of the remote-tab serve bridge, carried by the project
// groups PR so the tree can render a remote project's serve sessions while
// the full tab attach/pump pipeline lands with the sessions PR, which folds
// this file back into remote_tab.go.

// serveSessionEntry mirrors one GET /sessions row from the Serve.
type serveSessionEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Title      string `json:"title"`
	Turns      int    `json:"turns"`
	Current    bool   `json:"current"`
	MtimeMilli int64  `json:"mtimeMilli"`
}

// RemoteSessionView mirrors one serve /sessions entry on the frontend side.
type RemoteSessionView struct {
	Name           string `json:"name"`
	Title          string `json:"title,omitempty"`
	Turns          int    `json:"turns,omitempty"`
	Current        bool   `json:"current,omitempty"`
	LastActivityAt int64  `json:"lastActivityAt,omitempty"`
	Pinned         bool   `json:"pinned,omitempty"`
}

// serveDo issues a JSON request; the csrf guard rejects non-JSON POSTs.
func serveDo(ctx context.Context, client *http.Client, method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

// serveHandshake exchanges the pre-shared token for the session cookie.
// Serve replies 204 on success; the cookie lands in client's jar.
func serveHandshake(ctx context.Context, client *http.Client, base, token string) error {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return err
	}
	resp, err := serveDo(ctx, client, http.MethodPost, base+"/auth/token", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return fmt.Errorf("serve auth handshake: status %d", resp.StatusCode)
}

// serveSessions lists the serve's sessions.
func serveSessions(ctx context.Context, client *http.Client, base string) ([]serveSessionEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sessions", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serve /sessions: status %d", resp.StatusCode)
	}
	var out []serveSessionEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// serveClientForRef resolves an HTTP client for a host+workspace WITHOUT
// waking anything: a one-shot handshake against an already-ready serve
// registration. A serve that is not running reports an error — query paths
// must never cold-start one.
func (a *App) serveClientForRef(hostID, workspace string) (*http.Client, string, func(), error) {
	rt, err := a.remoteRT()
	if err != nil {
		return nil, "", nil, err
	}
	view, token, ok := rt.ServeSnapshot(hostID, workspace)
	if !ok {
		return nil, "", nil, fmt.Errorf("remote serve for %s:%s is not running", hostID, workspace)
	}
	ctx := a.bootContext()
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	jar, jarErr := cookiejar.New(nil)
	if jarErr != nil {
		cancel()
		return nil, "", nil, jarErr
	}
	client := &http.Client{Jar: jar}
	if err := serveHandshake(callCtx, client, view.LocalURL, token); err != nil {
		cancel()
		return nil, "", nil, err
	}
	return client, view.LocalURL, cancel, nil
}

// RemoteProjectSessions lists a remote project's serve sessions for the
// project tree. Live-tab fast paths, desktop title overrides and pinned
// synthesis arrive with the remote sessions PR.
func (a *App) RemoteProjectSessions(hostID, workspace string) ([]RemoteSessionView, error) {
	client, base, done, err := a.serveClientForRef(hostID, workspace)
	if err != nil {
		return nil, err
	}
	defer done()
	ctx := a.bootContext()
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	entries, err := serveSessions(callCtx, client, base)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteSessionView, 0, len(entries))
	for _, e := range entries {
		out = append(out, RemoteSessionView{
			Name:           e.Name,
			Title:          strings.TrimSpace(e.Title),
			Turns:          e.Turns,
			Current:        e.Current,
			LastActivityAt: e.MtimeMilli,
		})
	}
	return out, nil
}
