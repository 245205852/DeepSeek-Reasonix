package rpcwire

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// TestWriteStallFailsConnection covers the stdio-wedge case: the peer keeps
// the pipe open but never reads, so an unbounded write would block the caller
// forever. With MaxWriteStall set, the write aborts with WriteStallError and
// the connection fails, so later requests fail fast instead of queueing
// behind the stall.
func TestWriteStallFailsConnection(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close() // never read from pr: the pipe wedge

	conn := NewConn(pr, pw, Options{Name: "stall-test", MaxWriteStall: 50 * time.Millisecond})
	defer pw.Close()

	big := make(map[string]any)
	big["pad"] = string(make([]byte, 1<<20)) // 1 MiB, far beyond any pipe buffer

	start := time.Now()
	_, err := conn.Request(context.Background(), "never/answered", big)
	elapsed := time.Since(start)

	var stall *WriteStallError
	if !errors.As(err, &stall) {
		t.Fatalf("Request error = %v, want WriteStallError", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("stall took %s to abort, want close to 50ms", elapsed)
	}

	// The connection is terminal: the next request fails fast.
	_, err = conn.Request(context.Background(), "next/call", nil)
	if err == nil {
		t.Fatal("second request should fail on a terminal connection")
	}
	if elapsed2 := time.Since(start); elapsed2 > 5*time.Second {
		t.Fatalf("second request blocked for %s", elapsed2)
	}
}

// TestWriteCallerContextAbortLeavesConnectionAlive: a caller-side context
// deadline expiring mid-write aborts that request without killing the
// connection — a user cancel must not tear down a healthy transport.
func TestWriteCallerContextAbortLeavesConnectionAlive(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()

	conn := NewConn(pr, pw, Options{Name: "ctx-abort-test"})
	defer pw.Close()

	big := map[string]any{"pad": string(make([]byte, 1<<20))}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := conn.Request(ctx, "never/answered", big)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Request error = %v, want context.DeadlineExceeded", err)
	}
	// The connection itself was not failed by the caller-side abort: a small
	// write through a drained pipe still succeeds. Keep draining so the
	// aborted request's leftover bytes cannot block the notify write.
	go func() {
		_, _ = io.Copy(io.Discard, pr)
	}()
	if err := conn.Notify("ping", map[string]string{"ok": "1"}); err != nil {
		t.Fatalf("Notify after caller-abort: %v", err)
	}
}
