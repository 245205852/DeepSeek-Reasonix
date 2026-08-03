package reconnect

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"
)

func TestClassifyErrorRetryVsStop(t *testing.T) {
	if ClassifyError(errors.New("connection reset by peer")) != ClassRetry {
		t.Fatal("reset should retry")
	}
	if ClassifyError(errors.New("HOST_BUSY")) != ClassRetry {
		t.Fatal("HOST_BUSY should retry")
	}
	if ClassifyError(errors.New("host key fingerprint changed")) != ClassStop {
		t.Fatal("fingerprint must stop")
	}
	if ClassifyError(errors.New("build compatibility mismatch")) != ClassStop {
		t.Fatal("build id must stop")
	}
	if ClassifyError(ErrTargetMissing) != ClassStop {
		t.Fatal("missing target must stop")
	}
	if ClassifyError(errors.New("authentication failed for user")) != ClassStop {
		t.Fatal("auth must stop")
	}
	if ClassifyError(ErrStaleSlot) != ClassStop {
		t.Fatal("stale logical slot must stop")
	}
	if ClassifyError(context.Canceled) != ClassStop {
		t.Fatal("cancelled candidate must stop")
	}
}

func TestSupervisorImmediateThenBackoffSuccess(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	attempts := make(chan int, 3)
	waits := make(chan time.Duration, 2)
	sup := New(Options{
		Clock:   clock,
		RNG:     rand.New(rand.NewSource(1)), // full jitter with seed
		Window:  5 * time.Minute,
		Initial: time.Second,
		Max:     15 * time.Second,
		OnWait:  func(delay time.Duration) { waits <- delay },
	}, func(ctx context.Context, attempt int) error {
		attempts <- attempt
		if attempt < 3 {
			return errors.New("connection reset")
		}
		return nil
	})
	finished, started := sup.StartAsync(context.Background())
	if !started {
		t.Fatal("supervisor did not start")
	}
	if got := <-attempts; got != 1 {
		t.Fatalf("first attempt = %d", got)
	}
	<-waits // FakeClock waiter is registered before the hook fires.
	clock.Advance(time.Second)
	if got := <-attempts; got != 2 {
		t.Fatalf("second attempt = %d", got)
	}
	<-waits
	clock.Advance(2 * time.Second)
	if got := <-attempts; got != 3 {
		t.Fatalf("third attempt = %d", got)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after success")
	}
}

func TestSupervisorStopsOnTerminalError(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	calls := 0
	sup := New(Options{Clock: clock, RNG: rand.New(rand.NewSource(1))}, func(ctx context.Context, attempt int) error {
		calls++
		return errors.New("host key fingerprint changed")
	})
	sup.Start(context.Background())
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestSupervisorWindowExpiry(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	attempted := make(chan int, 1)
	waits := make(chan time.Duration, 1)
	sup := New(Options{
		Clock: clock, RNG: rand.New(rand.NewSource(1)),
		Window: 3 * time.Second, Initial: time.Second, Max: time.Second,
		OnWait: func(delay time.Duration) { waits <- delay },
	}, func(ctx context.Context, attempt int) error {
		attempted <- attempt
		return errors.New("eof")
	})
	finished, started := sup.StartAsync(context.Background())
	if !started {
		t.Fatal("supervisor did not start")
	}
	<-attempted
	<-waits
	clock.Advance(3 * time.Second)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("window did not end supervisor")
	}
}

func TestSupervisorCancel(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	started := make(chan struct{})
	sup := New(Options{Clock: clock, RNG: rand.New(rand.NewSource(1))}, func(ctx context.Context, attempt int) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	finished, startedOK := sup.StartAsync(context.Background())
	if !startedOK {
		t.Fatal("supervisor did not start")
	}
	<-started
	sup.Stop()
	<-finished
	if sup.Running() {
		t.Fatal("still running after stop")
	}
}
