package workspacelease

import (
	"context"
	"testing"
	"time"
)

func newOwnerWithGrace(t *testing.T, root, lockDir string, grace time.Duration) *Owner {
	t.Helper()
	owner, err := New(root, lockDir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	owner.graceAfter = grace
	return owner
}

func waitForRelease(t *testing.T, owner *Owner) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !owner.State().Acquired {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("workspace lease was never released")
}

// A resident background job (dev server, watcher) whose channel never closes
// must not own the workspace indefinitely once the session is idle.
func TestResidentBackgroundJobReleasesLeaseAfterGrace(t *testing.T) {
	root, lockDir := t.TempDir(), t.TempDir()
	owner := newOwnerWithGrace(t, root, lockDir, 20*time.Millisecond)

	owner.BeginRun()
	if err := owner.AcquireWrite(context.Background()); err != nil {
		t.Fatalf("AcquireWrite: %v", err)
	}
	resident := make(chan struct{})
	owner.RetainUntil(resident)
	owner.EndRun()

	waitForRelease(t, owner)

	other, err := New(root, lockDir, nil)
	if err != nil {
		t.Fatalf("New other: %v", err)
	}
	other.BeginRun()
	defer other.EndRun()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := other.AcquireWrite(ctx); err != nil {
		t.Fatalf("second session could not acquire the freed lease: %v", err)
	}

	// The job ending after the grace release must not release a second time.
	close(resident)
	time.Sleep(20 * time.Millisecond)
	if !other.State().Acquired {
		t.Fatal("the retained job released a lease it no longer owned")
	}
}

// The grace release must never fire underneath a run that has already started,
// which is the path that would let two sessions write concurrently.
func TestRunCancelsPendingGraceRelease(t *testing.T) {
	owner := newOwnerWithGrace(t, t.TempDir(), t.TempDir(), 20*time.Millisecond)

	owner.BeginRun()
	if err := owner.AcquireWrite(context.Background()); err != nil {
		t.Fatalf("AcquireWrite: %v", err)
	}
	resident := make(chan struct{})
	defer close(resident)
	owner.RetainUntil(resident)
	owner.EndRun()
	owner.BeginRun()
	defer owner.EndRun()

	time.Sleep(200 * time.Millisecond)
	if !owner.State().Acquired {
		t.Fatal("grace release fired while a run was active")
	}
}

// A background job that finishes on its own still releases immediately; the
// grace window is a ceiling, not an added delay.
func TestFinishedBackgroundJobReleasesWithoutWaitingForGrace(t *testing.T) {
	owner := newOwnerWithGrace(t, t.TempDir(), t.TempDir(), 30*time.Second)

	owner.BeginRun()
	if err := owner.AcquireWrite(context.Background()); err != nil {
		t.Fatalf("AcquireWrite: %v", err)
	}
	job := make(chan struct{})
	owner.RetainUntil(job)
	owner.EndRun()

	if !owner.State().Acquired {
		t.Fatal("lease released while the background job was still running")
	}
	close(job)
	waitForRelease(t, owner)
}
