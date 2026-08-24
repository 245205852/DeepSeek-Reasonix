package boot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/plugin"
)

// mcpHostSessionStub is a minimal Streamable-HTTP MCP server: enough to complete
// initialize + tools/list so the spec reaches pluginHost exactly as a real
// host-session server would.
func mcpHostSessionStub(t *testing.T, name string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": name, "version": "1"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name":        "ping",
				"description": "probe tool",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		default:
			http.Error(w, "unsupported method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}))
}

// A host-session MCP server (ACP session/new mcpServers) arrives as
// Options.ExtraPlugins and reaches pluginHost through eagerSpecs, so it connects
// and its tools show up in the capability catalog as ready. The capability
// runtime is a separate registry seeded only from cfg.Plugins, so before this
// fix every mcp-tool:<server>/<tool> dispatch was refused — a failure invisible
// to a happy-path turn and only reachable once the model actually called the
// tool mid-turn.
func TestBuildEnablesHostSessionMCPForCapabilityDispatch(t *testing.T) {
	isolateConfigHome(t)
	t.Chdir(robustTempDir(t))

	srv := mcpHostSessionStub(t, "acp-extra")
	defer srv.Close()

	ctrl, err := Build(context.Background(), Options{
		Sink: event.Discard,
		ExtraPlugins: []plugin.Spec{{
			Name:       "acp-extra",
			Type:       "http",
			URL:        srv.URL,
			Authorized: true,
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ctrl.Close()

	if !ctrl.CapabilityServerEnabled("acp-extra") {
		t.Fatal("host-supplied MCP server is not dispatchable through use_capability; " +
			"session/new mcpServers would connect and list tools but refuse every call")
	}
}

// A config server the user explicitly turned off must stay off, even while an
// unrelated host-session server is being enabled in the same boot. This is the
// invariant the enablement loop could plausibly break in a future refactor —
// widen it from "every extraSpec name" to "every name" and only this test
// notices.
func TestBuildLeavesDisabledConfigMCPUndispatchableAlongsideHostSession(t *testing.T) {
	isolateConfigHome(t)
	dir := robustTempDir(t)
	t.Chdir(dir)

	cfgSrv := mcpHostSessionStub(t, "config-off")
	defer cfgSrv.Close()
	extraSrv := mcpHostSessionStub(t, "acp-extra")
	defer extraSrv.Close()

	writeFile(t, dir, "reasonix.toml", `
[[plugins]]
name = "config-off"
type = "http"
url = "`+cfgSrv.URL+`"
auto_start = false
`)

	ctrl, err := Build(context.Background(), Options{
		Sink: event.Discard,
		ExtraPlugins: []plugin.Spec{{
			Name:       "acp-extra",
			Type:       "http",
			URL:        extraSrv.URL,
			Authorized: true,
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ctrl.Close()

	if !ctrl.CapabilityServerEnabled("acp-extra") {
		t.Error("the host-session server should be dispatchable")
	}
	if ctrl.CapabilityServerEnabled("config-off") {
		t.Error("a config server with auto_start = false must stay undispatchable; " +
			"enabling host-session servers must not enable anything else")
	}
}

// A host-session server takes precedence over a same-named config entry,
// including a disabled one: the client asked for that endpoint in this session,
// and it is the spec pluginHost connected and registered tools for.
//
// Asserted on the merge directly rather than through a booted Controller,
// because what must hold is which spec lands in the runtime registry — and the
// only way to observe that from outside would be to add a production accessor
// that exists solely for this test.
func TestMergeHostSessionCapabilitySpecsPrefersHostSessionOnNameCollision(t *testing.T) {
	autoStartOff := false
	configEntries := []config.PluginEntry{
		{Name: "shared-name", Type: "http", URL: "http://config.invalid", AutoStart: &autoStartOff},
		{Name: "config-only", Type: "http", URL: "http://config-only.invalid"},
	}
	configSpecs := []plugin.Spec{
		{Name: "shared-name", Type: "http", URL: "http://config.invalid"},
		{Name: "config-only", Type: "http", URL: "http://config-only.invalid"},
	}
	hostSession := []plugin.Spec{
		{Name: "shared-name", Type: "http", URL: "http://host-session.invalid"},
	}

	entries, specs := mergeHostSessionCapabilitySpecs(configEntries, configSpecs, hostSession)

	byName := map[string]plugin.Spec{}
	for _, spec := range specs {
		if _, dup := byName[spec.Name]; dup {
			t.Fatalf("%q appears twice; ConfigureServers would resolve it by slice order", spec.Name)
		}
		byName[spec.Name] = spec
	}
	if got := byName["shared-name"].URL; got != "http://host-session.invalid" {
		t.Errorf("collision resolved to %q, want the host-session endpoint", got)
	}
	if got := byName["config-only"].URL; got != "http://config-only.invalid" {
		t.Errorf("non-colliding config server = %q, want it untouched", got)
	}
	// The shadowed entry must go too, or ConfigureServers pairs auto_start=false
	// with the live host-session spec and shows it as neither auto-start nor
	// disabled.
	for _, entry := range entries {
		if entry.Name == "shared-name" {
			t.Error("shadowed config entry survived; it would be paired with the host-session spec")
		}
	}
	if len(entries) != 1 || entries[0].Name != "config-only" {
		t.Errorf("entries = %+v, want only config-only", entries)
	}
}

// With no host-session servers the inventory must be handed through untouched —
// the merge is not allowed to reorder or reallocate the ordinary config path.
func TestMergeHostSessionCapabilitySpecsIsIdentityWithoutHostSession(t *testing.T) {
	entriesIn := []config.PluginEntry{{Name: "a"}, {Name: "b"}}
	specsIn := []plugin.Spec{{Name: "a"}, {Name: "b"}}

	entries, specs := mergeHostSessionCapabilitySpecs(entriesIn, specsIn, nil)

	if len(entries) != len(entriesIn) || len(specs) != len(specsIn) {
		t.Fatalf("lengths changed: entries %d->%d, specs %d->%d",
			len(entriesIn), len(entries), len(specsIn), len(specs))
	}
	for i := range specs {
		if specs[i].Name != specsIn[i].Name || entries[i].Name != entriesIn[i].Name {
			t.Fatalf("order changed at %d", i)
		}
	}
}

// A name nobody supplied must stay undispatchable, so the fix above cannot be
// satisfied by defaulting unknown servers to enabled.
func TestBuildLeavesUnknownMCPServerUndispatchable(t *testing.T) {
	isolateConfigHome(t)
	t.Chdir(robustTempDir(t))

	ctrl, err := Build(context.Background(), Options{Sink: event.Discard})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ctrl.Close()

	if ctrl.CapabilityServerEnabled("never-configured") {
		t.Fatal("an unconfigured MCP server must not be dispatchable")
	}
}
