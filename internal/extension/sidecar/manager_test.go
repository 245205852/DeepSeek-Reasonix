package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/extension"
	"reasonix/internal/extension/protocol"
	"reasonix/internal/pluginpkg"
)

// installFakePlugin writes a v1 manifest for a fake sidecar package and
// registers it (enabled) in the pluginpkg installed state of home.
func installFakePlugin(t *testing.T, home, name string, configure func(rt *pluginpkg.RuntimeSpec)) {
	t.Helper()
	rt := fakeSidecarRuntime(t, configure)
	root := filepath.Join(home, "plugins", name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	manifest := map[string]any{
		"apiVersion": pluginpkg.ManifestAPIVersionV1,
		"name":       name,
		"version":    "1.0.0",
		"runtime":    rt,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, pluginpkg.NativeManifest), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	installed := pluginpkg.InstalledPlugin{
		Name:    name,
		Root:    pluginpkg.RelativeRoot(home, root),
		Version: "1.0.0",
		Enabled: true,
	}
	if err := pluginpkg.Upsert(home, installed); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

func TestManagerStartsEnabledRuntimePackages(t *testing.T) {
	home := t.TempDir()
	installFakePlugin(t, home, "alpha", func(rt *pluginpkg.RuntimeSpec) {
		rt.Intercepts = []string{"input.receive"}
		rt.Replaces = []string{"compaction"}
		rt.Capabilities = []string{"providers", "ui"}
		rt.Env[fakeEnvInitResult] = `{"protocolVersion":"1","name":"alpha","version":"1.0.0",` +
			`"subscriptions":["input.receive"],"replaces":["compaction"],` +
			`"providers":[{"ref":"plugin/alpha/openai/gpt-5"}],` +
			`"uiActions":[{"actionId":"act1"}],"stateSchemaVersion":0}`
	})

	manager, warnings, err := StartPackages(context.Background(), home, testSessionContext(), nil)
	if err != nil {
		t.Fatalf("StartPackages: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	t.Cleanup(func() { _ = manager.Close() })

	client := manager.Client("alpha")
	if client == nil {
		t.Fatal("manager has no client for alpha")
	}
	result, err := client.Intercept(context.Background(), protocol.EventInputReceive, json.RawMessage(`{}`), 5*time.Second)
	if err != nil || result.Decision != protocol.DecisionContinue {
		t.Fatalf("Intercept = %+v, %v", result, err)
	}

	// Declaration-level contributions: interceptor stub, strategy claim,
	// provider and UI action declarations.
	contributions := manager.Contributions()
	byKind := map[extension.ContributionKind][]extension.Contribution{}
	for _, c := range contributions {
		byKind[c.Kind] = append(byKind[c.Kind], c)
	}
	interceptors := byKind[extension.KindInterceptor]
	if len(interceptors) != 1 || interceptors[0].ID != "input.receive" {
		t.Fatalf("interceptor contributions = %+v", interceptors)
	}
	if interceptors[0].Source.PluginID != "alpha" || interceptors[0].Source.Scope != extension.ScopePlugin {
		t.Fatalf("interceptor source = %+v", interceptors[0].Source)
	}
	strategies := byKind[extension.KindStrategy]
	if len(strategies) != 1 || strategies[0].ID != "compaction" {
		t.Fatalf("strategy contributions = %+v", strategies)
	}
	claimer, ok := strategies[0].Payload.(extension.SlotClaimer)
	if !ok {
		t.Fatalf("strategy payload %T is not a SlotClaimer", strategies[0].Payload)
	}
	claims := extension.NewReplaceClaims()
	for _, slot := range claimer.ReplacementSlots() {
		if err := claims.Claim(slot, strategies[0].Source); err != nil {
			t.Fatalf("claim %q: %v", slot, err)
		}
	}
	providers := byKind[extension.KindProvider]
	if len(providers) != 1 || providers[0].ID != "openai/gpt-5" {
		t.Fatalf("provider contributions = %+v", providers)
	}
	uiActions := byKind[extension.KindUIAction]
	if len(uiActions) != 1 || uiActions[0].ID != "act1" {
		t.Fatalf("ui action contributions = %+v", uiActions)
	}
}

func TestManagerRequiredFailureFailsEverything(t *testing.T) {
	home := t.TempDir()
	installFakePlugin(t, home, "good-optional", nil)
	installFakePlugin(t, home, "bad-required", func(rt *pluginpkg.RuntimeSpec) {
		rt.Required = true
		rt.Env[fakeEnvInitResult] = `{"protocolVersion":"2","name":"bad","version":"1","stateSchemaVersion":0}`
	})
	manager, _, err := StartPackages(context.Background(), home, testSessionContext(), nil)
	if err == nil {
		t.Fatal("StartPackages succeeded with a broken required runtime")
	}
	var requiredErr *RequiredStartError
	if !errors.As(err, &requiredErr) {
		t.Fatalf("error %T is not a RequiredStartError", err)
	}
	if requiredErr.Plugin != "bad-required" {
		t.Fatalf("RequiredStartError.Plugin = %q", requiredErr.Plugin)
	}
	if manager != nil {
		t.Fatal("manager returned alongside the fatal error")
	}
}

func TestManagerOptionalFailureWarnsAndContinues(t *testing.T) {
	home := t.TempDir()
	installFakePlugin(t, home, "bad-optional", func(rt *pluginpkg.RuntimeSpec) {
		rt.Env[fakeEnvInitResult] = `{"protocolVersion":"2","name":"bad","version":"1","stateSchemaVersion":0}`
	})
	installFakePlugin(t, home, "good-optional", nil)
	manager, warnings, err := StartPackages(context.Background(), home, testSessionContext(), nil)
	if err != nil {
		t.Fatalf("StartPackages: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if len(warnings) != 1 || !strings.Contains(warnings[0], "bad-optional") {
		t.Fatalf("warnings = %v", warnings)
	}
	if manager.Client("bad-optional") != nil {
		t.Fatal("failed optional runtime has a client")
	}
	if manager.Client("good-optional") == nil {
		t.Fatal("healthy optional runtime has no client")
	}
}

func TestManagerDisabledPackageNeverLaunches(t *testing.T) {
	home := t.TempDir()
	installFakePlugin(t, home, "disabled-one", nil)
	if err := pluginpkg.SetEnabled(home, "disabled-one", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	manager, warnings, err := StartPackages(context.Background(), home, testSessionContext(), nil)
	if err != nil {
		t.Fatalf("StartPackages: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if got := len(manager.Clients()); got != 0 {
		t.Fatalf("disabled package produced %d clients", got)
	}
}

func TestManagerCloseShutsEverythingDown(t *testing.T) {
	home := t.TempDir()
	installFakePlugin(t, home, "one", nil)
	installFakePlugin(t, home, "two", func(rt *pluginpkg.RuntimeSpec) {
		rt.Env[fakeEnvMode] = "ignore_shutdown" // force the kill path
	})
	manager, _, err := StartPackages(context.Background(), home, testSessionContext(), nil)
	if err != nil {
		t.Fatalf("StartPackages: %v", err)
	}
	clients := manager.Clients()
	if len(clients) != 2 {
		t.Fatalf("clients = %d, want 2", len(clients))
	}
	start := time.Now()
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("manager close took %s", elapsed)
	}
	for _, client := range clients {
		if !client.Exited() {
			t.Fatalf("client %s still running after manager close", client.PluginID())
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
