package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"reasonix/internal/tool"
)

// appsFixtureServer advertises the Apps extension and serves tools with
// visibility/_meta.ui metadata, capturing tools/list as the client sees it.
type appsFixtureServer struct {
	mu      sync.Mutex
	listed  int
	cspSeen map[string][]string
}

func (f *appsFixtureServer) server(t *testing.T) *mcpsdk.Server {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "apps-fixture", Version: "1"}, nil)
	addAppTool := func(name string, meta map[string]any) {
		t := &mcpsdk.Tool{
			Name:        name,
			Description: name + " tool",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Meta:        mcpsdk.Meta(meta),
		}
		server.AddTool(t, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{
				Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: name + " done"}},
				StructuredContent: map[string]any{"ok": true, "tool": name},
			}, nil
		})
	}
	addAppTool("both_tool", map[string]any{})
	addAppTool("model_only", map[string]any{"visibility": []string{"model"}})
	addAppTool("app_only", map[string]any{
		"visibility":  []string{"app"},
		"resourceUri": "ui://legacy/index.html",
	})
	addAppTool("app_rich", map[string]any{
		"visibility": []string{"model", "app"},
		"ui": map[string]any{
			"resourceUri": "ui://app/rich.html",
			"csp":         map[string]any{"connect-src": []string{"https://api.example.com"}},
		},
	})
	return server
}

// startAppsClient connects a desktop-profile client through the real build
// path and returns the started Client plus the fixture for assertions.
func startAppsClient(t *testing.T) (*Host, *Client, toolCatalogSnapshot, *appsFixtureServer) {
	t.Helper()
	fixture := &appsFixtureServer{}
	host := NewHostWithProfile(HostProfileDesktopApps)
	lifeCtx, cancel := context.WithCancel(context.Background())
	transport := &sdkSessionTransport{
		name:            "apps-fixture",
		spec:            Spec{Name: "apps-fixture", Type: "http", StartupTimeout: 2 * time.Second},
		profile:         HostProfileDesktopApps,
		lifeCtx:         lifeCtx,
		cancel:          cancel,
		state:           SessionStateConnecting,
		reconnectDelays: []time.Duration{time.Millisecond},
	}
	transport.endpointFactory = func(ctx context.Context) (sdkEndpoint, error) {
		clientSide, serverSide := mcpsdk.NewInMemoryTransports()
		go func() { _ = fixture.server(t).Run(ctx, serverSide) }()
		return sdkEndpoint{transport: clientSide}, nil
	}
	t.Cleanup(transport.close)
	client := &Client{name: "apps-fixture", t: transport, spec: Spec{Name: "apps-fixture"}, transport: "http"}
	if _, err := client.listTools(t.Context()); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	client.toolsMu.RLock()
	snapshot := client.toolCatalog
	snapshot.infos = append([]ToolInfo(nil), snapshot.infos...)
	snapshot.adapters = append([]tool.Tool(nil), snapshot.adapters...)
	snapshot.appAdapters = append([]tool.Tool(nil), snapshot.appAdapters...)
	client.toolsMu.RUnlock()
	return host, client, snapshot, fixture
}

func TestMetaVisibilitySplitsCatalogs(t *testing.T) {
	_, _, catalog, _ := startAppsClient(t)

	var modelNames, appNames []string
	for _, tl := range catalog.adapters {
		modelNames = append(modelNames, tl.Name())
	}
	for _, tl := range catalog.appAdapters {
		appNames = append(appNames, tl.Name())
	}
	joined := strings.Join(modelNames, ",")
	for _, banned := range []string{"app_only"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("model catalog contains %s: %v", banned, modelNames)
		}
	}
	for _, want := range []string{"both_tool", "model_only", "app_rich"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("model catalog missing %s: %v", want, modelNames)
		}
	}
	appJoined := strings.Join(appNames, ",")
	for _, want := range []string{"both_tool", "app_only", "app_rich"} {
		if !strings.Contains(appJoined, want) {
			t.Fatalf("app catalog missing %s: %v", want, appNames)
		}
	}
	if strings.Contains(appJoined, "model_only") {
		t.Fatalf("app catalog contains model-only tool: %v", appNames)
	}
	// ToolInfo (use_capability list source) must also exclude app-only.
	for _, info := range catalog.infos {
		if info.Name == "app_only" {
			t.Fatal("app-only tool visible in ToolInfo list")
		}
	}
}

func TestMetaUIResourceNestedAndFlat(t *testing.T) {
	_, _, catalog, _ := startAppsClient(t)
	byName := map[string]tool.Tool{}
	for _, tl := range catalog.appAdapters {
		byName[tl.Name()] = tl
	}
	rich, ok := byName[toolName("apps-fixture", "app_rich")].(*remoteTool)
	if !ok || rich.UIResourceURI() != "ui://app/rich.html" {
		t.Fatalf("nested ui.resourceUri not parsed: %+v", rich)
	}
	if len(rich.UICSP()["connect-src"]) != 1 || rich.UICSP()["connect-src"][0] != "https://api.example.com" {
		t.Fatalf("csp not parsed: %v", rich.UICSP())
	}
	legacy, ok := byName[toolName("apps-fixture", "app_only")].(*remoteTool)
	if !ok || legacy.UIResourceURI() != "ui://legacy/index.html" {
		t.Fatalf("flat resourceUri fallback not parsed: %+v", legacy)
	}
}

func TestRichResultStampedOnCallContext(t *testing.T) {
	_, _, catalog, _ := startAppsClient(t)
	var rich *remoteTool
	for _, tl := range catalog.adapters {
		if rt, ok := tl.(*remoteTool); ok && rt.rawName == "app_rich" {
			rich = rt
			break
		}
	}
	if rich == nil {
		t.Fatal("app_rich not found")
	}
	ctx, collector := tool.WithMCPAppCollector(t.Context())
	out, _, err := rich.ExecuteWithImages(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "app_rich done") {
		t.Fatalf("text form lost: %q", out)
	}
	stamped := collector.Sanitized()
	if stamped == nil || stamped.Server == "" {
		t.Fatal("Apps presentation not collected")
	}
	if stamped.Server != "apps-fixture" || stamped.Tool != "app_rich" || stamped.ResourceURI != "ui://app/rich.html" {
		t.Fatalf("stamped identity = %+v", stamped)
	}
	if len(stamped.Structured) == 0 || !strings.Contains(string(stamped.Structured), `"tool":"app_rich"`) {
		t.Fatalf("structured content not captured: %s", stamped.Structured)
	}
}

func TestAppInstanceRegistryBoundAndReclaimed(t *testing.T) {
	host := NewHostWithProfile(HostProfileDesktopApps)
	reg := host.appInstances
	inst := host.RegisterAppInstance("srv", "tool", 3, "call-1", "ui://x/a.html")
	if len(inst.Token) != 48 {
		t.Fatalf("token length = %d, want 48 hex chars", len(inst.Token))
	}
	if got, ok := host.LookupAppInstance(inst.Token); !ok || got.Server != "srv" {
		t.Fatalf("lookup failed: %+v %v", got, ok)
	}
	for range maxAppInstances + 4 {
		host.RegisterAppInstance("srv", "tool", 3, "call", "ui://x/b.html")
	}
	if reg.Len() > maxAppInstances {
		t.Fatalf("registry exceeded bound: %d", reg.Len())
	}
	if _, ok := host.LookupAppInstance(inst.Token); ok {
		t.Fatal("oldest instance not evicted at capacity")
	}
	other := host.RegisterAppInstance("other", "t", 1, "c", "ui://y/a.html")
	host.appInstances.ReleaseServer("other")
	if _, ok := reg.Lookup(other.Token); ok {
		t.Fatal("release-server did not reclaim the server's instances")
	}
}
