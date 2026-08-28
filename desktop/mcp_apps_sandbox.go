package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"reasonix/internal/plugin"
)

// mcpAppsSandbox serves MCP Apps resources from per-server loopback origins:
// each Apps server gets its own 127.0.0.1 listener, so two servers' apps can
// never share cookies, storage, or an origin. The origin serves the outer
// sandbox relay page and proxies validated resources to the inner sandboxed
// iframe; the desktop webview never loads app HTML directly. A bind failure
// permanently degrades this desktop to the interactive MCP profile.
type mcpAppsSandbox struct {
	down atomic.Bool
	mu   sync.Mutex

	origins map[string]*mcpAppOrigin
}

type mcpAppOrigin struct {
	server   string
	listener net.Listener
	http     *http.Server
	nonce    string
}

// maxAppResourceBytes caps one decoded ui resource; maxAppPostMessageBytes
// caps one relayed frame.
const (
	maxAppResourceBytes    = 4 << 20
	maxAppPostMessageBytes = 8 << 20
	appResourceReadTimeout = 30 * time.Second
)

func (s *mcpAppsSandbox) noteUnavailable() { s.down.Store(true) }

func (s *mcpAppsSandbox) available() bool { return !s.down.Load() }

// appOriginURL returns the outer sandbox page URL for a server, binding the
// per-server listener on first use.
func (a *App) appOriginURL(server string) (string, error) {
	s := &a.mcpAppsSandbox
	if !s.available() {
		return "", fmt.Errorf("MCP Apps sandbox unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.origins == nil {
		s.origins = map[string]*mcpAppOrigin{}
	}
	if o, ok := s.origins[server]; ok {
		return o.relayURL(), nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.down.Store(true)
		return "", fmt.Errorf("bind MCP Apps origin: %w", err)
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		_ = ln.Close()
		return "", fmt.Errorf("app origin nonce: %w", err)
	}
	o := &mcpAppOrigin{server: server, listener: ln, nonce: hex.EncodeToString(nonceBytes)}
	o.http = &http.Server{
		Handler:           o.mux(a),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	s.origins[server] = o
	go func() { _ = o.http.Serve(ln) }()
	if a.ctx != nil {
		a.goSafe("mcpAppOrigin:"+server, func() {
			<-a.ctx.Done()
			_ = o.http.Close()
		})
	}
	return o.relayURL(), nil
}

func (o *mcpAppOrigin) relayURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/sandbox?nonce=%s", o.listener.Addr().(*net.TCPAddr).Port, o.nonce)
}

func (o *mcpAppOrigin) mux(a *App) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox", o.serveRelayPage)
	mux.HandleFunc("/resource", o.serveResource(a))
	return mux
}

// outerSandboxRelay is the only page served at the loopback origin: a relay
// between the desktop webview (parent) and the inner sandboxed iframe. It
// hardens the channel: no top navigation, popup, object, or download; the
// instance nonce binds the first parent message; every relayed frame checks
// event.source; frames above the cap are refused; RPC before the inner frame
// loads is dropped.
const outerSandboxRelay = `<!doctype html>
<html><head><meta charset="utf-8"><title>MCP App</title>
<script>
(function () {
  var params = new URLSearchParams(location.search);
  var nonce = params.get("nonce");
  var src = params.get("src");
  var inner = null;
  window.addEventListener("message", function (event) {
    if (event.data && event.data.__mcpInit === nonce && event.source === window.parent) {
      if (inner) return;
      inner = document.createElement("iframe");
      inner.setAttribute("sandbox", "allow-scripts");
      inner.setAttribute("src", src);
      document.body.appendChild(inner);
      return;
    }
    if (!inner || event.source !== inner.contentWindow) return;
    var text = typeof event.data === "string" ? event.data : JSON.stringify(event.data);
    if (text.length > %d) return;
    try { window.parent.postMessage(event.data, "*"); } catch (e) {}
  });
}());
</script></head><body></body></html>`

func (o *mcpAppOrigin) serveRelayPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Query().Get("nonce") != o.nonce {
		http.Error(w, "unknown instance", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-src 'self'; script-src 'unsafe-inline'")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	fmt.Fprintf(w, outerSandboxRelay, maxAppPostMessageBytes)
}

// serveResource validates the instance token, reads the ui resource from the
// MCP server, checks scheme/mime/size, and serves it with the declared CSP
// (deny-all default). Only the inner sandboxed iframe loads this.
func (o *mcpAppOrigin) serveResource(a *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host := a.activeMCPRuntimeHost()
		if host == nil {
			http.Error(w, "no runtime", http.StatusServiceUnavailable)
			return
		}
		inst, ok := host.LookupAppInstance(r.URL.Query().Get("token"))
		if !ok || inst.Server != o.server || !strings.HasPrefix(inst.ResourceURI, "ui://") {
			http.Error(w, "unknown instance", http.StatusForbidden)
			return
		}
		readCtx, cancel := context.WithTimeout(r.Context(), appResourceReadTimeout)
		defer cancel()
		content, mime, err := host.ReadResourceForApp(readCtx, inst.Server, inst.ResourceURI)
		if err != nil || len(content) > maxAppResourceBytes || !isAppHTMLMimeType(mime) {
			http.Error(w, "resource unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", appResourceCSP(host, inst))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-App-Sha256", resourceDigest(content))
		_, _ = io.WriteString(w, content)
	}
}

func isAppHTMLMimeType(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	return mime == "" || mime == "text/html" || strings.HasPrefix(mime, "text/html;") ||
		strings.Contains(mime, "profile=mcp-app")
}

// appResourceCSP: deny-all default, extended only by the tool's declared
// exact http/https origins; wildcards, credentials, and undeclared hosts are
// refused.
func appResourceCSP(host *plugin.Host, inst *plugin.AppInstance) string {
	directives := []string{
		"default-src 'none'", "script-src 'unsafe-inline'", "style-src 'unsafe-inline'",
		"img-src data:", "connect-src 'none'", "frame-ancestors 'self'",
	}
	if ref, ok := host.AppInstanceTool(inst.Token, inst.Tool); ok {
		var allowed []string
		for _, origin := range ref.UITool().UICSP()["connect-src"] {
			if cspOriginAllowed(origin) {
				allowed = append(allowed, origin)
			}
		}
		if len(allowed) > 0 {
			slices.Sort(allowed)
			directives[3] = "connect-src " + strings.Join(slices.Compact(allowed), " ")
		}
	}
	return strings.Join(directives, "; ")
}

func cspOriginAllowed(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" || strings.ContainsAny(origin, "*'") {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

func resourceDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
