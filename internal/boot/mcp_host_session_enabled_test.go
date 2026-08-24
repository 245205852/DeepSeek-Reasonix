package boot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
