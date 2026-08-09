package main

import (
	"testing"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/jobs"
)

func TestWaitDestroyHandlesPrefersBoundedWait(t *testing.T) {
	requested := make(chan time.Duration, 1)
	unboundedCalled := make(chan struct{}, 1)
	timedOut := waitDestroyHandles([]control.SessionDestroyHandle{{
		Wait: func() jobs.TeardownResult {
			unboundedCalled <- struct{}{}
			return jobs.TeardownResult{}
		},
		WaitFor: func(grace time.Duration) jobs.TeardownResult {
			requested <- grace
			return jobs.TeardownResult{}
		},
	}})
	if timedOut {
		t.Fatal("bounded wait reported an unexpected timeout")
	}
	select {
	case grace := <-requested:
		if grace != desktopSessionRemovalGrace {
			t.Fatalf("bounded wait grace = %s, want %s", grace, desktopSessionRemovalGrace)
		}
	default:
		t.Fatal("bounded wait was not called")
	}
	select {
	case <-unboundedCalled:
		t.Fatal("waitDestroyHandles used the unbounded wait when WaitFor was available")
	default:
	}
}

func TestWaitDestroyHandlesBoundsLegacyWait(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := time.Now()
	timedOut := waitDestroyHandles([]control.SessionDestroyHandle{{
		Wait: func() jobs.TeardownResult {
			<-release
			return jobs.TeardownResult{}
		},
	}})
	elapsed := time.Since(started)
	if !timedOut {
		t.Fatal("non-cooperative legacy wait did not report a timeout")
	}
	if elapsed < desktopSessionRemovalGrace || elapsed > 2*time.Second {
		t.Fatalf("legacy wait returned after %s, want a bounded wait near %s", elapsed, desktopSessionRemovalGrace)
	}
}
