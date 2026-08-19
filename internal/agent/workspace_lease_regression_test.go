package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"reasonix/internal/provider"
	"reasonix/internal/workspacelease"
)

type workspaceWritingHooks struct {
	path  string
	calls atomic.Int32
}

func (h *workspaceWritingHooks) PreToolUse(context.Context, string, json.RawMessage) (bool, string) {
	h.calls.Add(1)
	_ = os.WriteFile(h.path, []byte("hook"), 0o600)
	return false, ""
}

func (*workspaceWritingHooks) PostToolUse(context.Context, string, json.RawMessage, string) {}
func (*workspaceWritingHooks) PostToolUseFailure(context.Context, string, json.RawMessage, string, error) {
}
func (*workspaceWritingHooks) PostLLMCall(_ context.Context, reasoning string, _ int) string {
	return reasoning
}
func (*workspaceWritingHooks) HasPostLLMCall() bool                      { return false }
func (*workspaceWritingHooks) SubagentStop(context.Context, string)      {}
func (*workspaceWritingHooks) PreCompact(context.Context, string) string { return "" }

func TestWritableHooksUseWorkspaceLease(t *testing.T) {
	root, locks := t.TempDir(), t.TempDir()
	holder, _ := workspacelease.New(root, locks, nil)
	writerOwner, _ := workspacelease.New(root, locks, nil)
	protected := filepath.Join(root, "protected.go")
	releaseProtected, err := holder.HoldWriteForPath(context.Background(), protected)
	if err != nil {
		t.Fatal(err)
	}

	writer := &workspaceLeaseTestTool{name: "lease_reader", readOnly: true}
	hooks := &workspaceWritingHooks{path: protected}
	a := deliveryLeaseTestAgent(t, writerOwner, writer)
	a.writeWorkspaceRoot = root
	a.svc.hooks = hooks
	call := providerToolCall("write", writer.Name())
	call.Arguments = `{}`
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	out := a.executeOne(ctx, &a.turn, call)
	cancel()
	if !out.blocked || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("hook-capable writer was not blocked by active workspace write: %+v", out)
	}
	if got := hooks.calls.Load(); got != 0 {
		t.Fatalf("hook ran %d times before the workspace became exclusive", got)
	}

	releaseProtected()
	out = a.executeOne(context.Background(), &a.turn, call)
	if out.blocked || out.errMsg != "" {
		t.Fatalf("writer after release: %+v", out)
	}
	if got := hooks.calls.Load(); got != 1 {
		t.Fatalf("hook calls = %d, want 1", got)
	}
}

func TestWritableHooksReserveWholeParentWorkspace(t *testing.T) {
	root := t.TempDir()
	scheduler := NewSubagentScheduler(4, 2)
	hookClaim, err := NormalizeWritePaths(root, []string{"hook-side.go"})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &parentClaimProbeHooks{scheduler: scheduler, claim: hookClaim}
	writer := &recordingWriter{name: "lease_reader", readOnly: true}
	a := deliveryLeaseTestAgent(t, nil, writer)
	a.svc.hooks = hooks
	a.svc.writeScheduler = scheduler
	a.writeWorkspaceRoot = root
	call := providerToolCall("write", writer.Name())
	call.Arguments = `{}`
	out := a.executeOne(context.Background(), &a.turn, call)
	if out.blocked || out.errMsg != "" {
		t.Fatalf("executeOne failed: %+v", out)
	}
	if hooks.acquireErr == nil {
		t.Fatal("hook-side path bypassed the parent workspace reservation")
	}
}

func TestWritableHooksSerializeReadOnlyToolBatch(t *testing.T) {
	root := t.TempDir()
	first := &workspaceLeaseTestTool{name: "lease_reader_a", readOnly: true}
	second := &workspaceLeaseTestTool{name: "lease_reader_b", readOnly: true}
	hooks := &workspaceWritingHooks{path: filepath.Join(root, "hook-side.go")}
	a := deliveryLeaseTestAgent(t, nil, first, second)
	a.svc.writeScheduler = NewSubagentScheduler(4, 2)
	a.writeWorkspaceRoot = root
	calls := []provider.ToolCall{
		providerToolCall("read-a", first.Name()),
		providerToolCall("read-b", second.Name()),
	}

	if batches := a.toolCallBatches(calls); len(batches) != 1 || !batches[0].parallel {
		t.Fatalf("ordinary read batches = %+v, want unchanged parallel fan-out", batches)
	}
	a.svc.hooks = hooks
	batches := a.toolCallBatches(calls)
	if len(batches) != 1 || batches[0].parallel {
		t.Fatalf("hook-capable read batches = %+v, want one serial batch", batches)
	}
	result := a.executeBatch(context.Background(), &a.turn, calls)
	for i, outcome := range result.outcomes {
		if outcome.blocked || outcome.errMsg != "" {
			t.Fatalf("read-only call %d was dropped by hook coordination: %+v", i, outcome)
		}
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 || hooks.calls.Load() != 2 {
		t.Fatalf("executions first=%d second=%d hooks=%d, want 1/1/2",
			first.calls.Load(), second.calls.Load(), hooks.calls.Load())
	}
}
