package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/diff"
)

func TestRestoreCodeAllOrNothingOnMidPublishFailure(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	write(t, a, "a0")
	write(t, b, "b0")

	s := New("", root)
	s.Begin(0, "edit both", 0)
	s.Snapshot(diffChange(a, "a0"))
	s.Snapshot(diffChange(b, "b0"))
	write(t, a, "a1")
	write(t, b, "b1")

	plan, err := s.PrepareRewind(0, RewindCode, 1, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	plan.CanFiles = true
	plan.Conflicts = nil
	plan.DisabledReason = ""
	s.mu.Lock()
	s.plans[plan.PlanID] = preparedPlan{plan: plan, created: plan.CreatedAt}
	s.mu.Unlock()

	_, err = s.CommitRewindWithForward(plan.PlanID, nil, nil, &InjectFail{Phase: "publish_file", AfterFiles: 1})
	if err == nil {
		t.Fatal("expected injected failure")
	}

	if got := read(t, a); got != "a1" {
		t.Fatalf("a = %q, want a1 (compensated)", got)
	}
	if got := read(t, b); got != "b1" {
		t.Fatalf("b = %q, want b1 (compensated)", got)
	}
}

func TestRecoverCommittingTransaction(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "sess.ckpt")
	a := filepath.Join(root, "a.txt")
	write(t, a, "v0")

	s := New(dir, root)
	s.Begin(0, "p", 0)
	s.Snapshot(diffChange(a, "v0"))
	write(t, a, "v1")

	tx := &TransactionManifest{
		SchemaVersion: SchemaV2,
		ID:            "tx-crash",
		WorkspaceRoot: root,
		State:         TxCommitting,
		Kind:          "rewind",
		Turn:          0,
		Scope:         RewindCode,
		Targets: []TransactionTarget{{
			Path:           a,
			AbsPath:        a,
			Action:         "write",
			Published:      true,
			RestoreExisted: true,
			RestoreSHA:     Digest([]byte("v0")),
			ForwardExisted: true,
			ForwardSHA:     Digest([]byte("v1")),
		}},
	}
	ref, err := s.blobs.Put([]byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	tx.Targets[0].ForwardBlob = ref
	if err := os.WriteFile(a, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.persistTransaction(tx); err != nil {
		t.Fatal(err)
	}

	s2 := New(dir, root)
	_ = s2.RecoverTransactions()
	if got := read(t, a); got != "v1" {
		t.Fatalf("after recovery a = %q, want v1", got)
	}
}

func TestRecoverCrashAfterPublishBeforeProgressPersistence(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "sess.ckpt")
	a := filepath.Join(root, "a.txt")
	write(t, a, "before")
	s := New(dir, root)
	s.Begin(0, "edit", 0)
	s.CaptureBefore(a, CaptureBeforeOpts{Source: CaptureBeforeMutation})
	write(t, a, "after")
	s.CaptureAfter(a, CaptureAfterOpts{Seq: 1, Source: CaptureAfterMutation})

	plan, err := s.PrepareRewind(0, RewindCode, 1, 0, false)
	if err != nil || !plan.CanFiles {
		t.Fatalf("prepare: plan=%+v err=%v", plan, err)
	}
	if _, err := s.CommitRewindWithForward(plan.PlanID, nil, nil, &InjectFail{Phase: "after_publish_before_progress", AfterFiles: 0}); err == nil {
		t.Fatal("expected simulated crash")
	}
	if got := read(t, a); got != "before" {
		t.Fatalf("simulated crash did not occur after publish: %q", got)
	}

	_ = New(dir, root) // startup recovery runs while loading the store
	if got := read(t, a); got != "after" {
		t.Fatalf("crash recovery left partial rewind: got %q want after", got)
	}
}

func TestFileRevertRejectsStalePreviewEvenWithOldOverwriteApproval(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	write(t, a, "before")
	s := New("", root)
	s.Begin(0, "edit", 0)
	s.CaptureBefore(a, CaptureBeforeOpts{Source: CaptureBeforeMutation})
	write(t, a, "owned")
	s.CaptureAfter(a, CaptureAfterOpts{Seq: 1, Source: CaptureAfterMutation})

	plan, err := s.PrepareFileRevert(a, 1)
	if err != nil || !plan.CanFiles {
		t.Fatalf("prepare: plan=%+v err=%v", plan, err)
	}
	write(t, a, "external")
	if _, err := s.CommitFileRevert(plan.PlanID, ResolveOverwriteCheckpoint); err == nil {
		t.Fatal("stale overwrite approval must not authorize a later external edit")
	}
	if got := read(t, a); got != "external" {
		t.Fatalf("stale commit changed file to %q", got)
	}

	fresh, err := s.PrepareFileRevert(a, 1)
	if err != nil || len(fresh.Conflicts) == 0 {
		t.Fatalf("fresh preview should expose external conflict: plan=%+v err=%v", fresh, err)
	}
	result, err := s.CommitFileRevert(fresh.PlanID, ResolveOverwriteCheckpoint)
	if err != nil || !result.OK {
		t.Fatalf("fresh explicit overwrite failed: result=%+v err=%v", result, err)
	}
	if got := read(t, a); got != "before" {
		t.Fatalf("fresh confirmed revert = %q, want before", got)
	}
}

func TestUndoRestoresEmptyForwardFile(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	write(t, a, "before")
	s := New("", root)
	s.Begin(0, "empty", 0)
	s.CaptureBefore(a, CaptureBeforeOpts{Source: CaptureBeforeMutation})
	write(t, a, "")
	s.CaptureAfter(a, CaptureAfterOpts{Seq: 1, Source: CaptureAfterMutation})
	plan, err := s.PrepareRewind(0, RewindCode, 1, 0, false)
	if err != nil || !plan.CanFiles {
		t.Fatalf("prepare: plan=%+v err=%v", plan, err)
	}
	result, err := s.CommitRewindWithForward(plan.PlanID, nil, nil, nil)
	if err != nil || !result.OK {
		t.Fatalf("rewind: result=%+v err=%v", result, err)
	}
	undo, err := s.UndoRewind(result.TransactionID, nil)
	if err != nil || !undo.OK {
		t.Fatalf("undo: result=%+v err=%v", undo, err)
	}
	if got := read(t, a); got != "" {
		t.Fatalf("undo restored %q, want empty file", got)
	}
}

func TestPrecheckDetectsManualEdit(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	write(t, a, "v0")
	s := New("", root)
	s.Begin(0, "p", 0)
	s.CaptureBeforeFromChange(diffChange(a, "v0"), CaptureBeforeOpts{Source: CapturePreviewer})
	write(t, a, "v1")
	s.CaptureAfter(a, CaptureAfterOpts{Seq: 1, Source: CaptureAfterMutation})
	write(t, a, "manual")

	plan, err := s.PrepareRewind(0, RewindCode, 1, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CanFiles {
		t.Fatalf("expected CanFiles=false on manual edit, plan=%+v", plan)
	}
	if len(plan.Conflicts) == 0 {
		t.Fatal("expected conflicts")
	}
	if _, err := s.CommitRewindWithForward(plan.PlanID, nil, nil, nil); err == nil {
		t.Fatal("commit should fail")
	}
	if got := read(t, a); got != "manual" {
		t.Fatalf("a = %q, want manual", got)
	}
}

func TestTransactionCrashRecoveryPreparedIsAbandoned(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "sess.ckpt")
	s := New(dir, root)
	tx := &TransactionManifest{
		SchemaVersion: SchemaV2,
		ID:            "tx-prep",
		WorkspaceRoot: root,
		State:         TxPrepared,
		Kind:          "rewind",
	}
	if err := s.persistTransaction(tx); err != nil {
		t.Fatal(err)
	}
	_ = New(dir, root)
	var loaded TransactionManifest
	if err := readJSONFile(s.txManifestPath("tx-prep"), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.State != TxAborted {
		t.Fatalf("state = %s, want aborted", loaded.State)
	}
}

func diffChange(path, old string) diff.Change {
	return diff.Change{Path: path, Kind: diff.Modify, OldText: old}
}
