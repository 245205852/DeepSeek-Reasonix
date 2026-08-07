package control

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/extension"
)

func TestAdmitGuardedTurnRejectsDrainingGeneration(t *testing.T) {
	// Publish generation 2 so gen 1 is stale for admission.
	extension.DefaultPublishGate().Publish(2)

	var notices atomic.Int32
	c := New(Options{
		Sink: event.FuncSink(func(ev event.Event) {
			if ev.Kind == event.Notice {
				notices.Add(1)
			}
		}),
		RuntimeGeneration: 1,
	})
	// Ensure we don't leak a controller without Close.
	t.Cleanup(func() { c.Close() })

	got := c.runGuarded(func(context.Context) error {
		t.Fatal("body must not run on a draining generation")
		return nil
	})
	if got != turnDroppedDraining {
		t.Fatalf("admission = %v, want turnDroppedDraining", got)
	}
	// Give the notice a moment (emit is sync under sink).
	time.Sleep(time.Millisecond)
	if notices.Load() == 0 {
		t.Fatal("expected drain notice")
	}
	if extension.DefaultLifecycleMetrics.AdmissionRejected.Load() == 0 {
		t.Fatal("expected AdmissionRejected metric")
	}
}

func TestAdmitGuardedTurnAllowsPublishedGeneration(t *testing.T) {
	extension.DefaultPublishGate().Publish(9)
	c := New(Options{RuntimeGeneration: 9, Sink: event.Discard})
	t.Cleanup(func() { c.Close() })
	done := make(chan struct{})
	got := c.runGuarded(func(context.Context) error {
		close(done)
		return nil
	})
	if got != turnStarted {
		t.Fatalf("admission = %v, want turnStarted", got)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("turn body did not run")
	}
}
