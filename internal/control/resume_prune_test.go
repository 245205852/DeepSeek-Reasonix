package control

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

func coldResumeFixture(t *testing.T, threshold time.Duration) (*agent.Session, string) {
	t.Helper()

	dir := t.TempDir()
	loaded := &agent.Session{Messages: []provider.Message{
		{Role: provider.RoleSystem, Content: "sys"},
		{Role: provider.RoleUser, Content: "task"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "grep", Arguments: "{}"}}},
		{Role: provider.RoleTool, ToolCallID: "1", Name: "grep", Content: strings.Repeat("y", 5000)},
		{Role: provider.RoleAssistant, Content: "step done"},
		{Role: provider.RoleUser, Content: "next"},
		{Role: provider.RoleAssistant, Content: "ok"},
	}}
	path := agent.NewSessionPath(dir, "test")
	if err := loaded.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := agent.EnsureBranchMeta(path); err != nil {
		t.Fatalf("meta: %v", err)
	}

	exec := agent.New(nil, nil, agent.NewSession("sys"), agent.Options{ContextWindow: 1000, RecentKeep: 2, ArchiveDir: dir}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: dir, Label: "test"})
	if threshold == 0 {
		c.testCacheColdAfter = -1 // force cold
	} else {
		c.testCacheColdAfter = threshold
	}
	c.Resume(loaded, path)
	return loaded, path
}

func TestColdResumePrunesAndPersists(t *testing.T) {
	loaded, path := coldResumeFixture(t, 0)

	msgs := loaded.Snapshot()
	if !strings.HasPrefix(msgs[3].Content, "[elided tool result") {
		t.Fatalf("stale tool result not pruned on cold resume: %.60q", msgs[3].Content)
	}
	re, err := agent.LoadSession(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !strings.HasPrefix(re.Messages[3].Content, "[elided tool result") {
		t.Error("pruned transcript was not persisted")
	}
	if re.Messages[3].ToolCallID != "1" {
		t.Error("tool pairing lost in persisted transcript")
	}
}

func TestColdResumeAfterClonedHistoryStaysInPlace(t *testing.T) {

	dir := t.TempDir()
	saved := agent.NewSession("old sys")
	saved.Add(provider.Message{Role: provider.RoleUser, Content: "task"})
	saved.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "grep", Arguments: "{}"}}})
	saved.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "1", Name: "grep", Content: strings.Repeat("y", 5000)})
	saved.Add(provider.Message{Role: provider.RoleAssistant, Content: "step done"})
	saved.Add(provider.Message{Role: provider.RoleUser, Content: "next"})
	saved.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok"})
	path := agent.NewSessionPath(dir, "test")
	if err := saved.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := agent.EnsureBranchMeta(path); err != nil {
		t.Fatalf("meta: %v", err)
	}

	loaded, err := agent.LoadSession(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	msgs := loaded.Snapshot()
	msgs[0].Content = "new sys"
	resumed := loaded.CloneWithMessages(msgs)

	exec := agent.New(nil, nil, agent.NewSession("new sys"), agent.Options{ContextWindow: 1000, RecentKeep: 2, ArchiveDir: dir}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: dir, Label: "test"})
	c.testCacheColdAfter = -1 // force cold
	c.Resume(resumed, path)

	if got := c.SessionPath(); got != path {
		t.Fatalf("SessionPath after cold resume = %q, want %q", got, path)
	}
	re, err := agent.LoadSession(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := re.Messages[0].Content; got != "new sys" {
		t.Fatalf("system prompt after cold resume = %q, want new sys", got)
	}
	if !strings.HasPrefix(re.Messages[3].Content, "[elided tool result") {
		t.Fatalf("stale tool result not pruned on cloned cold resume: %.60q", re.Messages[3].Content)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "*-recovery-*.jsonl")); err != nil || len(matches) != 0 {
		t.Fatalf("recovery branches after cloned cold resume = %v err=%v, want none", matches, err)
	}
}

func TestWarmResumeLeavesHistoryAlone(t *testing.T) {
	loaded, path := coldResumeFixture(t, 24*time.Hour)

	if got := loaded.Snapshot()[3].Content; !strings.HasPrefix(got, "yyy") {
		t.Fatalf("warm resume rewrote history: %.60q", got)
	}
	re, err := agent.LoadSession(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !strings.HasPrefix(re.Messages[3].Content, "yyy") {
		t.Error("warm resume rewrote the saved transcript")
	}
}

func TestColdResumeCompactsToDigestPrefix(t *testing.T) {
	// C1 replay gate, cold branch: outside the cache window the resume must
	// NOT keep replaying the full history — it prunes stale tool results AND
	// runs a full compaction, so the first send is a small digest+tail prefix
	// instead of a full-price replay of ~1M cold tokens.
	dir := t.TempDir()
	big := strings.Repeat("work output ", 300) // large assistant/tool work
	oldDigest := "<compaction-summary>" + "\nold digest facts\n" + "</compaction-summary>"
	newestDigest := "<compaction-summary>" + "\nnewest digest\n" + "</compaction-summary>"
	saved := &agent.Session{Messages: []provider.Message{
		{Role: provider.RoleSystem, Content: "sys"},
		{Role: provider.RoleUser, Content: "task"},
		{Role: provider.RoleUser, Content: oldDigest},    // old digest → should be merged away
		{Role: provider.RoleUser, Content: newestDigest}, // newest digest → pinned
		{Role: provider.RoleAssistant, Content: big},
		{Role: provider.RoleTool, ToolCallID: "1", Name: "grep", Content: big},
		{Role: provider.RoleUser, Content: strings.Repeat("small turn ", 30)}, // 30 small user turns > 20 window
		{Role: provider.RoleAssistant, Content: "ok"},
	}}
	path := agent.NewSessionPath(dir, "test")
	if err := saved.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := agent.EnsureBranchMeta(path); err != nil {
		t.Fatalf("meta: %v", err)
	}

	loaded, err := agent.LoadSession(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	exec := agent.New(nil, nil, agent.NewSession("sys"), agent.Options{ContextWindow: 200000, RecentKeep: 2, ArchiveDir: dir}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: dir, Label: "test"})
	c.testCacheColdAfter = -1 // force cold
	c.Resume(loaded, path)

	msgs := loaded.Snapshot()
	var newestPinned bool
	for _, m := range msgs {
		if agent.IsCompactionSummary(m) && strings.Contains(m.Content, "newest digest") {
			newestPinned = true
		}
		if !agent.IsCompactionSummary(m) && strings.Contains(m.Content, "old digest facts") {
			t.Fatalf("old digest survived verbatim after cold resume compact: %+v", msgs)
		}
	}
	if !newestPinned {
		t.Fatalf("newest digest not pinned after cold resume compact: %+v", msgs)
	}
	if len(msgs) > 10 {
		t.Fatalf("cold resume left %d messages — expected a compacted digest+tail prefix", len(msgs))
	}
}

func TestWarmResumeSkipsCompaction(t *testing.T) {
	// C1 replay gate, warm branch: inside the cache window the resume replays
	// the history as-is — no prune, no compaction — so the knowledge graph
	// survives verbatim and the warm prefix hits the server cache cheaply.
	loaded, path := coldResumeFixture(t, 24*time.Hour)
	if got := loaded.Snapshot()[3].Content; !strings.HasPrefix(got, "yyy") {
		t.Fatalf("warm resume rewrote history: %.60q", got)
	}
	re, err := agent.LoadSession(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !strings.HasPrefix(re.Messages[3].Content, "yyy") {
		t.Error("warm resume rewrote the saved transcript")
	}
}
