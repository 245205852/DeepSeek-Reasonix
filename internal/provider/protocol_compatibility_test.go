package provider

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func freshProtocolHints() *protocolHints {
	return &protocolHints{entries: make(map[[32]byte]*protocolHint)}
}

func TestProtocolHintsBoundedConcurrentAndExpiring(t *testing.T) {
	hints := freshProtocolHints()
	key := sha256.Sum256([]byte("path"))
	now := time.Now()
	hints.failedHTTP2(key, "INTERNAL_ERROR", now)
	var probes atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if hints.selectHTTP1(key, now) != nil {
				probes.Add(1)
			}
		})
	}
	wg.Wait()
	if probes.Load() != 1 {
		t.Fatalf("concurrent probes = %d", probes.Load())
	}
	old := hints.entries[key]
	hints.settle(key, old, "active")
	if hints.selectHTTP1(key, now) != old {
		t.Fatal("successful path not reused")
	}
	hints.settle(key, old, "pending") // late cancellation must not undo success
	if old.mode != "active" {
		t.Fatal("late cancellation reset active hint")
	}
	hints.settle(key, old, "disabled")
	hints.settle(key, old, "active") // late success must not undo failure
	hints.failedHTTP2(key, "PROTOCOL_ERROR", now.Add(time.Minute))
	if hints.selectHTTP1(key, now.Add(time.Minute)) != nil || old.reason != "INTERNAL_ERROR" {
		t.Fatal("failed compatibility was rearmed or overwritten")
	}
	next := now.Add(protocolCompatibilityTTL)
	if hints.selectHTTP1(key, next) != nil {
		t.Fatal("expired hint remained selected")
	}
	hints.failedHTTP2(key, "PROTOCOL_ERROR", next)
	hints.settle(key, old, "disabled")
	if hints.selectHTTP1(key, next) == nil {
		t.Fatal("old completion disabled new generation")
	}
	for i := range protocolCompatibilityLimit + 10 {
		k := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		hints.failedHTTP2(k, "INTERNAL_ERROR", next)
	}
	if len(hints.entries) != protocolCompatibilityLimit {
		t.Fatalf("unbounded hint count: %d", len(hints.entries))
	}
}

func TestProtocolFailureOnlyUsesTypedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"internal", http2.StreamError{Code: http2.ErrCodeInternal}, true},
		{"protocol", http2.ConnectionError(http2.ErrCodeProtocol), true},
		{"requires-http1", http2.StreamError{Code: http2.ErrCodeHTTP11Required}, true},
		{"wording", errors.New("HTTP/2 INTERNAL_ERROR secret-token"), false},
		{"eof", io.ErrUnexpectedEOF, false},
		{"api", &APIError{Status: 401, Body: "INTERNAL_ERROR"}, false},
		{"cancel", context.Canceled, false},
		{"refused", http2.StreamError{Code: http2.ErrCodeRefusedStream}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hints := freshProtocolHints()
			tr := &protocolTransport{hints: hints}
			tr.failed(t.Context(), [32]byte{}, nil, tc.err)
			if got := hints.selectHTTP1([32]byte{}, time.Now()) != nil; got != tc.want {
				t.Fatalf("selected=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestProtocolFailureNeverBecomesRetryableReset(t *testing.T) {
	for _, err := range []error{
		errors.Join(http2.StreamError{Code: http2.ErrCodeInternal}, io.ErrUnexpectedEOF),
		&protocolCompatibilityError{io.EOF},
		&protocolCompatibilityError{io.ErrUnexpectedEOF},
		&protocolCompatibilityError{&RequestSendTimeoutError{}},
	} {
		if IsConnReset(err) || ClassifyRecovery(err).Retryable {
			t.Fatalf("terminal protocol failure became retryable: %T", err)
		}
	}
}
