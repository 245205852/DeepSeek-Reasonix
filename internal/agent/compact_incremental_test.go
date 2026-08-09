package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"reasonix/internal/ablation"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// foldAgent builds an agent whose session grows past the verbatim tail every
// generation, so each CompactNow has something to fold.
func foldAgent(t *testing.T, prov provider.Provider, incremental bool) (*Agent, *Session) {
	t.Helper()
	sess := NewSession("sys")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "task"})
	a := New(prov, tool.NewRegistry(), sess, Options{
		ContextWindow: 128_000,
		RecentKeep:    2,
		SessionPath:   t.TempDir() + "/session.jsonl",
		ArchiveDir:    t.TempDir(),
		Ablation:      foldArmSet(incremental),
	}, event.Discard)
	return a, sess
}

func foldArmSet(incremental bool) ablation.Set {
	if incremental {
		return ablation.New(ablation.FullFold)
	}
	return ablation.Set{}
}

func addWork(sess *Session, gen int) {
	for i := range 8 {
		id := fmt.Sprintf("g%dc%d", gen, i)
		sess.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: id, Name: "read_file", Arguments: "{}"}}})
		sess.Add(provider.Message{Role: provider.RoleTool, ToolCallID: id, Name: "read_file",
			Content: strings.Repeat(fmt.Sprintf("gen %d line %d\n", gen, i), 1200)})
	}
}

// foldInput runs one generation and returns everything that fold sent the
// summarizer, joined — a fold may take several calls, and the question is what
// it read in total.
func foldInput(t *testing.T, a *Agent, sess *Session, prov *countingProvider, gen int) string {
	t.Helper()
	addWork(sess, gen)
	prov.got = nil
	if err := a.CompactNow(context.Background(), ""); err != nil {
		t.Fatalf("gen %d: %v", gen, err)
	}
	if len(prov.got) == 0 {
		t.Fatalf("gen %d folded nothing", gen)
	}
	var b strings.Builder
	for _, req := range prov.got {
		for _, m := range req.Messages {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

func TestFullFoldRereadsTheOldestHistoryEveryTime(t *testing.T) {
	// The default arm is what keeps digests from chaining: a later fold still
	// re-reads the oldest raw work rather than a summary of it.
	prov := &countingProvider{reply: "digest"}
	a, sess := foldAgent(t, prov, false)
	var last string
	for gen := range 3 {
		last = foldInput(t, a, sess, prov, gen)
	}
	if !strings.Contains(last, "gen 0 line 0") {
		t.Fatal("a full fold must re-read the oldest raw history")
	}
	if strings.Contains(last, summaryTagOpen) {
		t.Fatal("a full fold must not feed its own prior digest back in")
	}
}

func TestIncrementalFoldFeedsThePriorDigestInsteadOfRereading(t *testing.T) {
	prov := &countingProvider{reply: "digest"}
	a, sess := foldAgent(t, prov, true)
	var last string
	for gen := range 3 {
		last = foldInput(t, a, sess, prov, gen)
	}
	if !strings.Contains(last, summaryTagOpen) {
		t.Fatalf("an incremental fold must feed the prior digest back in:\n%.200q", last)
	}
	if strings.Contains(last, "gen 0 line 0") {
		t.Fatal("an incremental fold must not re-read history the prior digest already covers")
	}
}

func TestIncrementalFoldKeepsTheProjectionValidAgainstCanonical(t *testing.T) {
	// The fold reads the projection, but its coverage bookkeeping still indexes
	// the canonical transcript — otherwise the next turn rebuilds from scratch.
	prov := &countingProvider{reply: "digest"}
	a, sess := foldAgent(t, prov, true)
	for gen := range 2 {
		foldInput(t, a, sess, prov, gen)
	}
	canonical, version := a.session.snapshotMessagesVersion()
	st := a.compactionState
	if st.Projection.CoveredCount != len(canonical) {
		t.Fatalf("CoveredCount = %d, want %d (the canonical length)", st.Projection.CoveredCount, len(canonical))
	}
	if !projectionValid(st, canonical, version, a.currentPromptCacheKey()) {
		t.Fatal("incremental fold left the projection invalid against the canonical transcript")
	}
	if visible := a.modelVisibleMessages(); len(visible) >= len(canonical) {
		t.Fatalf("model-visible = %d messages vs canonical %d; the fold saved nothing", len(visible), len(canonical))
	}
}

func TestIncrementalFoldFallsBackToCanonicalWithoutAProjection(t *testing.T) {
	prov := &countingProvider{reply: "digest"}
	a, sess := foldAgent(t, prov, true)
	if first := foldInput(t, a, sess, prov, 0); !strings.Contains(first, "gen 0 line 0") {
		t.Fatal("the first fold has no projection to read and must fold the canonical transcript")
	}
}
