package agent

import (
	"context"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/extension/protocol"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func TestAgentBeforeStartToolCountUsesStableSchemas(t *testing.T) {
	goalTool, ok := tool.LookupBuiltin("update_goal")
	if !ok {
		t.Fatal("update_goal builtin not registered")
	}
	reg := tool.NewRegistry()
	reg.Add(goalTool)

	run := func(ctx context.Context) dispatch.AgentStartPayload {
		t.Helper()
		client := &fakeDispatchClient{}
		d := newExtDispatcher(client, true, nil, extension.PointAgentBeforeStart)
		mp := &mockProvider{name: "p", chunks: []provider.Chunk{
			{Type: provider.ChunkText, Text: "hi"}, {Type: provider.ChunkDone},
		}}
		a := New(mp, reg, NewSession("sys"), Options{Extensions: d}, event.Discard)
		if err := a.Run(ctx, "hello"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var payload dispatch.AgentStartPayload
		if !client.interceptPayloadFor(protocol.EventAgentBeforeStart, &payload) {
			t.Fatal("agent.before_start did not fire")
		}
		return payload
	}

	if got := run(context.Background()).ToolCount; got != 1 {
		t.Fatalf("ordinary ToolCount = %d, want stable update_goal schema", got)
	}
	ctx := tool.WithGoalTurnRecorder(context.Background(), requestGoalRecorder{})
	if got := run(ctx).ToolCount; got != 1 {
		t.Fatalf("Goal ToolCount = %d, want stable update_goal schema", got)
	}
}
