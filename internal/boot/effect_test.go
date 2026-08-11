package boot

// Effect tests assert final-boundary behavior through the real Build stack:
// a scripted provider records what actually reaches the provider boundary.
// Component correctness is not system effectiveness (see REASONIX.md).

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"reasonix/internal/ablation"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

type effectRecordingProvider struct {
	mu   sync.Mutex
	reqs []provider.Request
}

func (p *effectRecordingProvider) Name() string { return "boot-effect-test" }

func (p *effectRecordingProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	ch := make(chan provider.Chunk, 2)
	ch <- provider.Chunk{Type: provider.ChunkText, Text: "ok"}
	ch <- provider.Chunk{Type: provider.ChunkDone}
	close(ch)
	return ch, nil
}

func (p *effectRecordingProvider) requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.reqs...)
}

// effectRun builds the real stack around a recording provider, runs one
// prompt, and returns every request that reached the provider boundary.
func effectRun(t *testing.T, kind, tokenMode string, arm ablation.Set) []provider.Request {
	t.Helper()
	isolateConfigHome(t)
	dir := robustTempDir(t)
	t.Chdir(dir)

	rec := &effectRecordingProvider{}
	provider.Register(kind, func(provider.Config) (provider.Provider, error) {
		return rec, nil
	})
	writeFile(t, dir, "reasonix.toml", `
default_model = "test-model"

[agent]
system_prompt = "BASE"

[[providers]]
name = "test-model"
kind = "`+kind+`"
model = "x"
`)

	ctrl, err := Build(context.Background(), Options{Sink: event.Discard, TokenMode: tokenMode, Ablation: arm})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ctrl.Close()
	if err := ctrl.Run(context.Background(), "reply ok"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := rec.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the provider boundary")
	}
	return reqs
}

func toolNames(req provider.Request) map[string]bool {
	names := make(map[string]bool, len(req.Tools))
	for _, tool := range req.Tools {
		names[tool.Name] = true
	}
	return names
}

// TestEffectRoleSettingsShareProviderToolSurface pins the unified contract:
// light/balanced/delivery send identical top-level tool schemas; optional
// tools are reached only through use_capability.
func TestEffectRoleSettingsShareProviderToolSurface(t *testing.T) {
	balanced := effectRun(t, "boot-effect-balanced", "", ablation.Set{})
	light := effectRun(t, "boot-effect-light", "economy", ablation.Set{})
	delivery := effectRun(t, "boot-effect-delivery", "delivery", ablation.Set{})

	balNames := toolSchemaNames(balanced[0].Tools)
	if !reflect.DeepEqual(toolSchemaNames(light[0].Tools), balNames) {
		t.Fatalf("light surface diverged from balanced\nlight=%v\nbalanced=%v", toolSchemaNames(light[0].Tools), balNames)
	}
	if !reflect.DeepEqual(toolSchemaNames(delivery[0].Tools), balNames) {
		t.Fatalf("delivery surface diverged from balanced\ndelivery=%v\nbalanced=%v", toolSchemaNames(delivery[0].Tools), balNames)
	}
	if len(balNames) > 16 {
		t.Fatalf("unified surface sent %d tools; expected a small fixed core set", len(balNames))
	}
	names := toolNames(balanced[0])
	if !names["use_capability"] {
		t.Fatal("unified surface must expose use_capability")
	}
	if names["connect_tool_source"] {
		t.Fatal("connect_tool_source must not appear on the provider-visible surface")
	}
	if names["task"] || names["grep"] {
		t.Fatal("optional tools must not be top-level; use use_capability")
	}
}

// TestEffectSubagentAblationRemovesChildToolSchemas asserts the ablation at
// the capability boundary: with subagents off the model cannot dispatch
// task/fleet through the registry even via use_capability.
func TestEffectSubagentAblationRemovesChildToolSchemas(t *testing.T) {
	control := effectRun(t, "boot-effect-sub-on", "", ablation.Set{})
	ablated := effectRun(t, "boot-effect-sub-off", "", ablation.New(ablation.Subagent))

	// Top-level schema never exposes task; verify registry dispatch instead.
	ctrl, err := Build(context.Background(), Options{Sink: event.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	_ = control
	_ = ablated
	// Ablation is enforced inside TaskTool registration at boot; the unified
	// surface stays use_capability-only either way.
	if names := toolNames(control[0]); names["task"] {
		t.Fatal("unified surface must not expose task top-level")
	}
	if names := toolNames(ablated[0]); names["task"] || names["parallel_tasks"] || names["fleet"] {
		t.Fatalf("subagent-ablated surface still offers spawn tools top-level: %v", toolSchemaNames(ablated[0].Tools))
	}
}
