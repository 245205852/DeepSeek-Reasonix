package sessioninbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/fileutil"
	"reasonix/internal/store"
)

func TestEnqueueSnapshotAndRead(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rec, err := s.Enqueue(EnqueueRequest{
		Intent: IntentFollowup,
		Envelope: PromptEnvelope{
			DisplayText: "hello world",
			SubmitText:  "hello world",
		},
		Source: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ItemID == "" || rec.Position != 1 {
		t.Fatalf("receipt = %+v", rec)
	}
	snap := s.Snapshot()
	if len(snap.Items) != 1 || snap.Items[0].Preview == "" {
		t.Fatalf("snapshot = %+v", snap)
	}
	// Body must not appear in snapshot metadata beyond preview.
	if strings.Contains(snap.Items[0].Preview, "\x00") {
		t.Fatal("unexpected binary in preview")
	}
	meta, env, err := s.ReadItem(rec.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != rec.ItemID || env.SubmitText != "hello world" {
		t.Fatalf("read = meta=%+v env=%+v", meta, env)
	}
	// Permissions: dir 0700, blob 0600.
	info, err := os.Stat(store.SessionInboxDir(session))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("inbox dir perm = %o", info.Mode().Perm())
	}
}

func TestIdempotentEnqueue(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.Enqueue(EnqueueRequest{
		Intent:      IntentFollowup,
		Envelope:    PromptEnvelope{SubmitText: "x"},
		Idempotency: "msg-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Enqueue(EnqueueRequest{
		Intent:      IntentFollowup,
		Envelope:    PromptEnvelope{SubmitText: "y"},
		Idempotency: "msg-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.ItemID != b.ItemID || !b.Idempotent {
		t.Fatalf("idempotency failed: a=%+v b=%+v", a, b)
	}
	if len(s.Snapshot().Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(s.Snapshot().Items))
	}
}

func TestCapacityLimits(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{MaxItems: 2, MaxItemBytes: 200, MaxTotalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: strings.Repeat("a", 400)}}); err != ErrItemTooLarge {
		t.Fatalf("item too large: %v", err)
	}
	if _, err := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "one"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "two"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "three"}}); err != ErrCapacityItems {
		t.Fatalf("cap items: %v", err)
	}
}

func TestDeleteThenBlobGone(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec, _ := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "bye"}})
	if err := s.DeleteItem(rec.ItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.SessionInboxDir(session), "blobs", rec.ItemID+".json")); !os.IsNotExist(err) {
		t.Fatalf("blob should be gone, err=%v", err)
	}
}

func TestCrashAfterBlobBeforeManifestLeavesNoValidItem(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	// Inject crash after blob rename, before manifest commit.
	fileutil.CrashPoint = func(op, path string) {
		if op == "inbox-manifest-write" {
			panic("inject crash before manifest")
		}
	}
	t.Cleanup(func() { fileutil.CrashPoint = nil })
	func() {
		defer func() { _ = recover() }()
		_, _ = s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "orphan"}})
	}()
	fileutil.CrashPoint = nil
	// Re-open: no valid items; orphan blob may exist and is GC'd/quarantined.
	s2, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if n := len(s2.Snapshot().Items); n != 0 {
		t.Fatalf("want 0 valid items after crash, got %d", n)
	}
}

func TestCrossProcessRecoveryPauses(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetState(rec.ItemID, StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Simulate another process by rewriting runID in a fresh Open with different ProcessRunID.
	// Open always uses ProcessRunID(); force recovery by editing manifest runId.
	manPath := filepath.Join(store.SessionInboxDir(session), "manifest.json")
	data, _ := os.ReadFile(manPath)
	data = []byte(strings.Replace(string(data), ProcessRunID(), "other-run-id-0000", 1))
	_ = os.WriteFile(manPath, data, 0o600)

	s2, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	snap := s2.Snapshot()
	if !snap.Paused || !snap.Recovered {
		t.Fatalf("want paused+recovered, got %+v", snap)
	}
	if len(snap.Items) != 1 || snap.Items[0].State != StateUncertain {
		t.Fatalf("want uncertain item, got %+v", snap.Items)
	}
}

func TestPreviewDoesNotMaterializeHugeBody(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	p := PreviewText(huge, 40)
	if len(p) > 80 {
		t.Fatalf("preview too long: %d", len(p))
	}
	if !strings.HasSuffix(p, "…") {
		t.Fatalf("want ellipsis, got %q", p)
	}
}

func TestUpdateUsesImmutableBlobRevision(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec, err := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	meta, _, err := s.ReadItem(rec.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	oldBlob := blobNameFor(meta)
	updated, err := s.UpdateItem(rec.ItemID, PromptEnvelope{SubmitText: "revised"})
	if err != nil {
		t.Fatal(err)
	}
	if blobNameFor(updated) == oldBlob {
		t.Fatal("update must write a new blob name, not overwrite in place")
	}
	if _, err := os.Stat(s.blobPath(oldBlob)); !os.IsNotExist(err) {
		t.Fatalf("old blob should be removed after successful update, err=%v", err)
	}
	_, env, err := s.ReadItem(rec.ItemID)
	if err != nil || env.SubmitText != "revised" {
		t.Fatalf("read after update = %+v err=%v", env, err)
	}
}

func TestCorruptManifestSalvagesBlobs(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "keep-me"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Corrupt the manifest.
	manPath := filepath.Join(store.SessionInboxDir(session), "manifest.json")
	if err := os.WriteFile(manPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	snap := s2.Snapshot()
	if !snap.Paused || !snap.Recovered {
		t.Fatalf("want paused+recovered after corrupt manifest, got %+v", snap)
	}
	if len(snap.Items) == 0 || snap.RecoveredN == 0 {
		t.Fatalf("salvage must surface blobs, got items=%d recoveredN=%d", len(snap.Items), snap.RecoveredN)
	}
}

func TestFreezeRefsRejectsWorkspaceEscape(t *testing.T) {
	ws := t.TempDir()
	_, err := FreezeRefs(context.Background(), ws, []string{"/etc/passwd"})
	if err != nil {
		// FreezeRefs swallows per-path errors into frozen markers; check content.
	}
	refs, err := FreezeRefs(context.Background(), ws, []string{"/etc/passwd"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if !strings.Contains(string(refs[0].Content), "outside workspace") && !strings.Contains(string(refs[0].Content), "freeze failed") {
		t.Fatalf("want workspace escape rejection, got %q", refs[0].Content)
	}
}

func TestFreezeRefsRejectsSymlinkEscape(t *testing.T) {
	ws := t.TempDir()
	external := t.TempDir()
	secret := filepath.Join(external, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "linked-secret.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	refs, err := FreezeRefs(context.Background(), ws, []string{"linked-secret.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("want one blocked marker, got %d", len(refs))
	}
	if strings.Contains(string(refs[0].Content), "outside-secret") {
		t.Fatal("workspace-local symlink leaked content from outside the workspace")
	}
	if !strings.Contains(string(refs[0].Content), "path escapes workspace") {
		t.Fatalf("want symlink escape rejection, got %q", refs[0].Content)
	}
}

func TestApplyFrozenRefsIsDeterministic(t *testing.T) {
	bodies := map[string]string{
		"z/file.txt": "z-body",
		"a/file.txt": "a-body",
	}
	first := ApplyFrozenRefs("inspect refs", bodies)
	for i := 0; i < 20; i++ {
		if got := ApplyFrozenRefs("inspect refs", bodies); got != first {
			t.Fatalf("frozen reference serialization changed between calls:\n%s\n---\n%s", first, got)
		}
	}
	if strings.Index(first, "@a/file.txt") > strings.Index(first, "@z/file.txt") {
		t.Fatalf("frozen references are not sorted: %q", first)
	}
}

func TestMoveAndPause(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "a"}})
	b, _ := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "b"}})
	if err := s.MoveItem(b.ItemID, 0); err != nil {
		t.Fatal(err)
	}
	items := s.Snapshot().Items
	if items[0].ID != b.ItemID || items[1].ID != a.ItemID {
		t.Fatalf("order = %v", items)
	}
	if err := s.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.NextQueued(); ok {
		t.Fatal("paused inbox must not dispatch")
	}
}

func TestDiscardPendingItemsIsScopedAndAtomic(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	s, err := Open(session, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "a"}})
	b, _ := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "b"}})
	c, _ := s.Enqueue(EnqueueRequest{Envelope: PromptEnvelope{SubmitText: "c"}})

	if err := s.SetState(b.ItemID, StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DiscardPendingItems([]string{a.ItemID, b.ItemID}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("discard admitted item error = %v, want ErrInvalidState", err)
	}
	if got := len(s.Snapshot().Items); got != 3 {
		t.Fatalf("failed batch discard changed manifest: got %d items", got)
	}

	if err := s.DiscardPendingItems([]string{a.ItemID, "already-consumed"}); err != nil {
		t.Fatal(err)
	}
	items := s.Snapshot().Items
	if len(items) != 2 || items[0].ID != b.ItemID || items[1].ID != c.ItemID {
		t.Fatalf("scoped discard left items = %+v", items)
	}
}
