package agent

import (
	"context"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

type nativeContextEditingProvider struct {
	*fakeProvider
}

func (p *nativeContextEditingProvider) ContextEditingCapabilities() provider.ContextEditingCapabilities {
	return provider.ContextEditingCapabilities{
		NativeToolUseClear: true,
		PolicyVersion:      "clear_tool_uses_test",
	}
}

func TestContextEditingResolvesProviderCapabilityBeforeRequestAndCacheLineage(t *testing.T) {
	opts := Options{
		ContextEditing:      "native",
		ContextWindow:       100_000,
		MaxOutputTokens:     1_024,
		WorkspaceID:         "workspace",
		ModelRef:            "model",
		ToolResultSnipRatio: 0.6,
	}
	sess := &Session{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hello"}}}
	native := New(&nativeContextEditingProvider{fakeProvider: &fakeProvider{}}, tool.NewRegistry(), sess, opts, event.Discard)
	if native.requestedContextEditing != "native" || native.contextEditing != "native" {
		t.Fatalf("native modes = requested %q effective %q", native.requestedContextEditing, native.contextEditing)
	}
	prepared, err := native.prepareSamplingRequest(context.Background())
	if err != nil {
		t.Fatalf("prepare native request: %v", err)
	}
	policy := prepared.req.ContextEditing
	if policy == nil || policy.Mode != "native" || policy.KeepToolUses != 3 || policy.ClearToolInputs {
		t.Fatalf("native policy = %+v", policy)
	}
	if policy.TriggerInputTokens != 60_000 || policy.ClearAtLeastInputTokens != 4_096 {
		t.Fatalf("native thresholds = trigger %d min %d", policy.TriggerInputTokens, policy.ClearAtLeastInputTokens)
	}
	if key := native.currentPromptCacheKey(); !strings.Contains(key, "clear_tool_uses_test-t60000-k3-m4096-ifalse") {
		t.Fatalf("native cache lineage does not include the effective policy: %q", key)
	}

	unsupported := New(&fakeProvider{}, tool.NewRegistry(), NewSession(""), opts, event.Discard)
	localOpts := opts
	localOpts.ContextEditing = "local"
	local := New(&fakeProvider{}, tool.NewRegistry(), NewSession(""), localOpts, event.Discard)
	if unsupported.requestedContextEditing != "native" || unsupported.contextEditing != "local" {
		t.Fatalf("unsupported modes = requested %q effective %q", unsupported.requestedContextEditing, unsupported.contextEditing)
	}
	if unsupported.currentPromptCacheKey() != local.currentPromptCacheKey() {
		t.Fatalf("unsupported native split cache lineage: %q != local %q", unsupported.currentPromptCacheKey(), local.currentPromptCacheKey())
	}
	unsupportedPrepared, err := unsupported.prepareSamplingRequest(context.Background())
	if err != nil {
		t.Fatalf("prepare unsupported request: %v", err)
	}
	if unsupportedPrepared.req.ContextEditing != nil {
		t.Fatalf("unsupported provider received native policy: %+v", unsupportedPrepared.req.ContextEditing)
	}
}

func TestUnsupportedNativeContextEditingNoticeIsOneShot(t *testing.T) {
	sink := &recordSink{}
	a := New(&fakeProvider{}, tool.NewRegistry(), NewSession(""), Options{
		ContextEditing:  "native",
		ContextWindow:   100_000,
		MaxOutputTokens: 1_024,
	}, sink)
	for range 2 {
		if _, err := a.prepareSamplingRequest(context.Background()); err != nil {
			t.Fatalf("prepare request: %v", err)
		}
	}
	var notices int
	for _, got := range sink.kinds(event.Notice) {
		if got.Code == event.NoticeCodeContextEditingFallback {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("fallback notices = %d, want 1", notices)
	}
}

func TestNativeContextEditingReplacesLocalToolMaintenanceButKeepsFullFold(t *testing.T) {
	newAgent := func() (*Agent, *fakeProvider, *Session) {
		fake := &fakeProvider{reply: "summary"}
		sess := pruneFixture(strings.Repeat("line\n", 1000))
		a := New(&nativeContextEditingProvider{fakeProvider: fake}, tool.NewRegistry(), sess, Options{
			ContextEditing: "native",
			ContextWindow:  1_000,
			RecentKeep:     2,
			ArchiveDir:     t.TempDir(),
		}, event.Discard)
		return a, fake, sess
	}

	a, fake, sess := newAgent()
	prepareForObservedUsage(a, context.Background(), &provider.Usage{PromptTokens: 650})
	if fake.got != nil {
		t.Fatal("summarizer was called in the native tool-clear pressure band")
	}
	if got := a.currentProjectionVersion(); got != 0 {
		t.Fatalf("native mode installed a local snip projection, version = %d", got)
	}
	if got := sess.Snapshot()[3].Content; strings.HasPrefix(got, snippedMarker) || strings.HasPrefix(got, prunedMarker) {
		t.Fatalf("native mode rewrote canonical tool result: %.80q", got)
	}

	a, fake, _ = newAgent()
	prepareForObservedUsage(a, context.Background(), &provider.Usage{PromptTokens: 850})
	if fake.got == nil {
		t.Fatal("native mode suppressed the full-fold fallback above the fold trigger")
	}
}

func TestNativeContextEditingAppliedEditWritesUnifiedReceipt(t *testing.T) {
	sink := &recordSink{}
	a := New(&nativeContextEditingProvider{fakeProvider: &fakeProvider{}}, tool.NewRegistry(), NewSession("system"), Options{
		ContextEditing: "native", ContextWindow: 100_000,
	}, sink)
	a.contextManager().ObserveUsage(&provider.Usage{
		PromptTokens: 25_000, ContextEditingType: "clear_tool_uses_20250919",
		ContextEditingClearedToolUses: 8, ContextEditingClearedTokens: 50_000,
	})
	snapshot := a.ContextMaintenanceSnapshot()
	if snapshot.LastReceipt == nil || snapshot.LastReceipt.Action != "native_tool_clear" ||
		snapshot.LastReceipt.SavedTokens != 50_000 || snapshot.LastReceipt.AffectedToolResults != 8 {
		t.Fatalf("native maintenance receipt = %+v", snapshot.LastReceipt)
	}
	if got := sink.kinds(event.ContextMaintenanceEvent); len(got) != 1 || got[0].Maintenance.SavedTokens != 50_000 {
		t.Fatalf("native maintenance events = %+v", got)
	}
	// Duplicate observation for the same request is idempotent.
	a.contextManager().ObserveUsage(&provider.Usage{
		PromptTokens: 25_000, ContextEditingType: "clear_tool_uses_20250919",
		ContextEditingClearedToolUses: 8, ContextEditingClearedTokens: 50_000,
	})
	if got := len(sink.kinds(event.ContextMaintenanceEvent)); got != 1 {
		t.Fatalf("duplicate native maintenance events = %d, want 1", got)
	}
}

func TestTaskSubagentsPreserveRequestedContextEditingForTheirProvider(t *testing.T) {
	task := NewTaskToolWithOptions(TaskToolOptions{
		Provider:       &fakeProvider{},
		ParentRegistry: tool.NewRegistry(),
		ContextEditing: "native",
	})
	opts := task.subagentOptions(context.Background(), 5, nil, 100_000, 1, "child", nil)
	if opts.ContextEditing != "native" {
		t.Fatalf("subagent ContextEditing = %q, want requested native", opts.ContextEditing)
	}

	child := New(&fakeProvider{}, tool.NewRegistry(), NewSession(""), opts, event.Discard)
	if child.contextEditing != "local" {
		t.Fatalf("incompatible child effective mode = %q, want local", child.contextEditing)
	}
}
