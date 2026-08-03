// Package reconnect implements the Remote Workbench auto-recovery supervisor.
// It uses an injectable clock and RNG so tests can drive full-jitter backoff
// deterministically without wall-clock sleeps.
package reconnect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// Defaults from the recovery plan.
const (
	DefaultInitialBackoff = time.Second
	DefaultMaxBackoff     = 15 * time.Second
	DefaultWindow         = 5 * time.Minute
)

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// WallClock uses the process clock.
type WallClock struct{}

func (WallClock) Now() time.Time                         { return time.Now() }
func (WallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FakeClock is a controllable clock for tests.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	when time.Time
	ch   chan time.Time
}

func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{when: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the fake clock forward and releases due waiters.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due []fakeWaiter
	var pending []fakeWaiter
	for _, w := range c.waiters {
		if !w.when.After(now) {
			due = append(due, w)
		} else {
			pending = append(pending, w)
		}
	}
	c.waiters = pending
	c.mu.Unlock()
	for _, w := range due {
		w.ch <- now
	}
}

// Class is the reconnect error classification.
type Class int

const (
	// ClassRetry means the supervisor should back off and try again.
	ClassRetry Class = iota
	// ClassStop means automatic recovery must stop (auth, fingerprint, missing target, ...).
	ClassStop
)

// ClassifyError maps transport and recovery errors to retry vs stop.
func ClassifyError(err error) Class {
	if err == nil {
		return ClassRetry
	}
	if errors.Is(err, ErrStaleSlot) || errors.Is(err, context.Canceled) {
		return ClassStop
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassRetry
	}
	msg := strings.ToLower(err.Error())
	// Terminal conditions (hard stop).
	terminal := []string{
		"authentication failed",
		"auth failed",
		"permission denied",
		"host key",
		"fingerprint",
		"build id",
		"build compatibility",
		"schema hash",
		"protocol version",
		"provider authorization",
		"provider trust",
		"not authorized",
		"session not found",
		"target not found",
		"runtime target missing",
		"workspace is not open",
		"unknown remote host",
		"host key changed",
		"remote target missing",
	}
	for _, needle := range terminal {
		if strings.Contains(msg, needle) {
			return ClassStop
		}
	}
	// Explicit retryable markers.
	retryable := []string{
		"eof",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"timeout",
		"temporary",
		"reconnecting",
		"host_busy",
		"stale_connection",
		"stale connection",
		"connection refused",
		"network is unreachable",
		"no route to host",
		"use of closed network connection",
	}
	for _, needle := range retryable {
		if strings.Contains(msg, needle) {
			return ClassRetry
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return ClassRetry
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ClassRetry
	}
	// Unknown errors retry within the window; permanent product errors should
	// use explicit wording above.
	return ClassRetry
}

// AttemptFunc performs one candidate reconnect. It must be failure-atomic:
// failed candidates must not destroy the last visible snapshot.
type AttemptFunc func(ctx context.Context, attempt int) error

// Options configures the supervisor.
type Options struct {
	Clock  Clock
	RNG    *rand.Rand
	Window time.Duration
	// Initial is the first backoff ceiling after the immediate first attempt.
	Initial time.Duration
	Max     time.Duration
	// Factor is the exponential base (default 2).
	Factor float64
	// OnWait is an optional deterministic-test hook invoked after the clock wait
	// is registered. Production callers leave it nil.
	OnWait func(time.Duration)
}

func (o Options) normalize() Options {
	if o.Clock == nil {
		o.Clock = WallClock{}
	}
	if o.RNG == nil {
		o.RNG = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.Initial <= 0 {
		o.Initial = DefaultInitialBackoff
	}
	if o.Max <= 0 {
		o.Max = DefaultMaxBackoff
	}
	if o.Factor < 1 {
		o.Factor = 2
	}
	return o
}

// Supervisor runs automatic Remote recovery for one logical slot.
type Supervisor struct {
	opts    Options
	attempt AttemptFunc

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	done    chan struct{}
}

// New builds a supervisor. attempt is invoked for each candidate connection.
func New(opts Options, attempt AttemptFunc) *Supervisor {
	return &Supervisor{opts: opts.normalize(), attempt: attempt}
}

// Start begins recovery and blocks until success, ClassStop, cancel, or the
// recovery window elapses. Callers that need a background supervisor should
// invoke Start from their own goroutine. The first attempt runs immediately;
// subsequent attempts use full-jitter exponential backoff.
func (s *Supervisor) Start(parent context.Context) {
	ctx, _, ok := s.begin(parent)
	if !ok {
		return
	}
	s.loop(ctx)
}

// StartAsync publishes the running/cancel state synchronously before launching
// the loop. This closes the Start-vs-Stop gap that exists when callers wrap
// Start in a goroutine themselves.
func (s *Supervisor) StartAsync(parent context.Context) (<-chan struct{}, bool) {
	ctx, done, ok := s.begin(parent)
	if !ok {
		return done, false
	}
	go s.loop(ctx)
	return done, true
}

func (s *Supervisor) begin(parent context.Context) (context.Context, <-chan struct{}, bool) {
	s.mu.Lock()
	if s.running {
		done := s.done
		s.mu.Unlock()
		return nil, done, false
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.running = true
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()
	return ctx, done, true
}

// Stop cancels the supervisor and waits for the loop to exit.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	running := s.running
	s.mu.Unlock()
	if !running {
		return
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Running reports whether a loop is active.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Supervisor) loop(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		close(s.done)
		s.mu.Unlock()
	}()
	opts := s.opts
	deadline := opts.Clock.Now().Add(opts.Window)
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		attempt++
		err := s.attempt(ctx, attempt)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if ClassifyError(err) == ClassStop {
			return
		}
		if !opts.Clock.Now().Before(deadline) {
			return
		}
		// Immediate first try already ran; subsequent waits use full-jitter.
		ceil := backoffCeiling(opts, attempt-1)
		// attempt-1: after first failure, ceiling is Initial (1s); then 2s, 4s, 8s, max 15s.
		delay := time.Duration(opts.RNG.Int63n(int64(ceil) + 1))
		if remaining := deadline.Sub(opts.Clock.Now()); delay > remaining {
			delay = remaining
		}
		if delay < 0 {
			return
		}
		wait := opts.Clock.After(delay)
		if opts.OnWait != nil {
			opts.OnWait(delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-wait:
		}
		if !opts.Clock.Now().Before(deadline) {
			return
		}
	}
}

func backoffCeiling(opts Options, failureIndex int) time.Duration {
	// failureIndex 0 → Initial; 1 → Initial*Factor; capped at Max.
	d := float64(opts.Initial)
	for i := 0; i < failureIndex; i++ {
		d *= opts.Factor
		if d >= float64(opts.Max) {
			return opts.Max
		}
	}
	if d >= float64(opts.Max) {
		return opts.Max
	}
	return time.Duration(d)
}

// ErrTargetMissing is returned when the exact RuntimeTarget is absent on the Host.
var ErrTargetMissing = errors.New("remote target missing: exact session is no longer present on the Host")

// ErrStaleSlot stops a supervisor whose logical Remote slot was replaced or
// explicitly detached. It is not a transport failure and must never retry.
var ErrStaleSlot = errors.New("stale logical remote slot")

// FormatAttemptError builds a non-secret status string.
func FormatAttemptError(attempt int, err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("attempt %d: %s", attempt, truncateErr(err, 160))
}

func truncateErr(err error, n int) string {
	msg := strings.TrimSpace(err.Error())
	if len(msg) <= n {
		return msg
	}
	return msg[:n] + "…"
}
