package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtensionGenerationBumpsOnMCPMutationSites(t *testing.T) {
	// Guard the mutation sites that publish shared Host / config changes while
	// controller builds may still be off-lock. Missing a bump reintroduces stale
	// tool registries after Install/Update/Remove/Reconnect.
	raw, err := os.ReadFile(filepath.Join("app.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, name := range []string{
		"func (a *App) InstallMCPServer",
		"func (a *App) UpdateMCPServer",
		"func (a *App) RemoveMCPServer",
		"func (a *App) ReconnectMCPServer",
	} {
		idx := strings.Index(src, name)
		if idx < 0 {
			t.Fatalf("missing %s", name)
		}
		// Inspect the function body until the next top-level App method.
		rest := src[idx:]
		next := strings.Index(rest[len(name):], "\nfunc (a *App) ")
		body := rest
		if next >= 0 {
			body = rest[:len(name)+next]
		}
		if !strings.Contains(body, "a.bumpExtensionGeneration()") {
			t.Fatalf("%s does not bump extensionGeneration", name)
		}
	}
}
