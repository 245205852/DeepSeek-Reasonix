package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/plugin"
)

func TestMCPAuthCLIUsesReasonixPrivateState(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Chdir(workspace)
	entry := config.PluginEntry{Name: "figma", Type: "http", URL: "https://mcp.figma.com/mcp", Source: config.MCPSourceUserConfig}
	if _, err := config.InstallUserPluginForRoot(workspace, entry, true); err != nil {
		t.Fatal(err)
	}

	previous := mcpAuthorizeForCLI
	mcpAuthorizeForCLI = func(_ context.Context, spec plugin.Spec, openURL func(string) error) error {
		if spec.Name != "figma" || spec.URL != entry.URL {
			t.Fatalf("authorization spec = %+v", spec)
		}
		if spec.StateDir == "" || strings.HasPrefix(filepath.Clean(spec.StateDir), filepath.Clean(workspace)+string(filepath.Separator)) {
			t.Fatalf("OAuth state dir must be private Reasonix state, got %q", spec.StateDir)
		}
		if spec.OAuthHTTPClient == nil || openURL == nil {
			t.Fatal("authorization requires the proxy-aware client and browser opener")
		}
		return nil
	}
	t.Cleanup(func() { mcpAuthorizeForCLI = previous })
	if code := mcpAuthCLI([]string{"figma"}); code != 0 {
		t.Fatalf("mcp auth exit = %d", code)
	}
}
