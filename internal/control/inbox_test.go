package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/sessioninbox"
)

func TestEnqueueInboxDurableAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Options{SessionPath: session, SessionDir: dir, Sink: event.Discard})
	rec, err := c.EnqueueInbox(InboxRequest{
		Intent:  sessioninbox.IntentFollowup,
		Display: "hello durable",
		Submit:  "hello durable",
		Source:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ItemID == "" {
		t.Fatal("empty item id")
	}
	snap := c.InboxSnapshot()
	if len(snap.Items) != 1 || snap.Items[0].Preview == "" {
		t.Fatalf("snapshot = %+v", snap)
	}
	// Body only via ReadInboxItem.
	_, env, err := c.ReadInboxItem(rec.ItemID)
	if err != nil || env.SubmitText != "hello durable" {
		t.Fatalf("read = %+v err=%v", env, err)
	}
}

func TestTrySteerRejectedBecomesFollowup(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	c := New(Options{SessionPath: session, SessionDir: dir, Sink: event.Discard})
	rec, err := c.EnqueueInbox(InboxRequest{
		Intent: sessioninbox.IntentSteer,
		Submit: "mid-turn please",
	})
	if err != nil {
		t.Fatal(err)
	}
	// No running turn → reject, keep as follow-up.
	got, err := c.TrySteerInboxItem(rec.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disposition != sessioninbox.DispositionQueuedFollowup {
		t.Fatalf("disposition = %s, want queued_followup", got.Disposition)
	}
	meta, _, err := c.ReadInboxItem(rec.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.State != sessioninbox.StateQueued || meta.Intent != sessioninbox.IntentFollowup {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestIdempotentEnqueue(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	c := New(Options{SessionPath: session, SessionDir: dir, Sink: event.Discard})
	a, err := c.EnqueueInbox(InboxRequest{Submit: "x", Idempotency: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.EnqueueInbox(InboxRequest{Submit: "y", Idempotency: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ItemID != b.ItemID || !b.Idempotent {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
}

func TestMultiSteerActiveSetAcksAll(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	c := New(Options{SessionPath: session, SessionDir: dir, Sink: event.Discard})

	st, err := c.ensureInbox()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		rec, err := c.EnqueueInbox(InboxRequest{Submit: "body-" + string(rune('a'+i))})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ItemID)
		_ = st.SetState(rec.ItemID, sessioninbox.StateSteerConsumed, "")
	}
	c.inbox.mu.Lock()
	c.inbox.clearActive()
	for _, id := range ids {
		c.inbox.trackActive(id)
	}
	c.inbox.mu.Unlock()

	c.onInboxTurnDone()
	if n := len(c.InboxSnapshot().Items); n != 0 {
		t.Fatalf("want all 3 steers acked/dequeued, still have %d items", n)
	}
}

func TestSubmitInboxUsesFrozenReferenceWithoutLiveReresolve(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	refPath := filepath.Join(workspace, "note.txt")
	if err := os.WriteFile(refPath, []byte("enqueue-time-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(dir, "s.jsonl")
	sess := agent.NewSession("sys")
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	sink, done, _ := collectSink()
	c := New(Options{
		Runner:        appendingRunner{session: sess},
		Executor:      exec,
		Sink:          sink,
		SessionDir:    dir,
		SessionPath:   sessionPath,
		WorkspaceRoot: workspace,
	})
	defer c.autosaveWG.Wait()

	rec, err := c.EnqueueInbox(InboxRequest{Submit: "review @note.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refPath, []byte("live-body-after-enqueue"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := c.TrySubmitInboxItem(rec.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disposition != sessioninbox.DispositionStarted {
		t.Fatalf("disposition = %q, want started", got.Disposition)
	}
	waitForDone(t, done)

	messages := sess.Snapshot()
	if len(messages) < 2 {
		t.Fatalf("messages = %+v", messages)
	}
	input := messages[len(messages)-1].Content
	if !strings.Contains(input, "enqueue-time-body") {
		t.Fatalf("prepared inbox turn omitted frozen body: %q", input)
	}
	if strings.Contains(input, "live-body-after-enqueue") {
		t.Fatalf("prepared inbox turn re-resolved live reference: %q", input)
	}
	if strings.Count(input, "enqueue-time-body") != 1 {
		t.Fatalf("frozen body injected more than once: %q", input)
	}
}

func TestCancelWithInboxItemsDiscardsOnlyOwnedPendingItems(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	c := New(Options{SessionPath: session, SessionDir: dir, Sink: event.Discard})
	owned, err := c.EnqueueInbox(InboxRequest{Submit: "owned by composer"})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := c.EnqueueInbox(InboxRequest{Submit: "owned by bot"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CancelWithInboxItems([]string{owned.ItemID}); err != nil {
		t.Fatal(err)
	}
	snap := c.InboxSnapshot()
	if snap.Paused {
		t.Fatal("successful scoped cancel left inbox paused")
	}
	if len(snap.Items) != 1 || snap.Items[0].ID != unrelated.ItemID {
		t.Fatalf("scoped cancel left items = %+v", snap.Items)
	}
}
