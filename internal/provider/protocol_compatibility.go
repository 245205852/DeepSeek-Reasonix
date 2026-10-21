package provider

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const protocolCompatibilityTTL = 10 * time.Minute
const protocolCompatibilityLimit = 256

// A hint is process-local and survives provider reconstruction, but never
// persists URLs, proxy credentials or configuration. Fixed expiry prevents an
// unhealthy path from permanently disabling HTTP/2.
var protocolCompatibilityHints = &protocolHints{entries: make(map[[32]byte]*protocolHint)}

type protocolHint struct {
	until  time.Time
	reason string
	mode   string // pending, probing, active, disabled
}

type protocolHints struct {
	mu      sync.Mutex
	entries map[[32]byte]*protocolHint
}

func (s *protocolHints) selectHTTP1(key [32]byte, now time.Time) *protocolHint {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.entries[key]
	if h == nil {
		return nil
	}
	if !now.Before(h.until) {
		delete(s.entries, key)
		return nil
	}
	switch h.mode {
	case "pending":
		h.mode = "probing" // only one concurrent request tries an unproven path
	case "active":
	default:
		return nil
	}
	return h
}

func (s *protocolHints) failedHTTP2(key [32]byte, code string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, h := range s.entries {
		if !now.Before(h.until) {
			delete(s.entries, k)
		}
	}
	if s.entries[key] != nil || len(s.entries) >= protocolCompatibilityLimit {
		return // late failures cannot reset a newer decision or extend its TTL
	}
	s.entries[key] = &protocolHint{until: now.Add(protocolCompatibilityTTL), reason: code, mode: "pending"}
}

func (s *protocolHints) settle(key [32]byte, h *protocolHint, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[key] == h && h.mode != "disabled" && (mode != "pending" || h.mode == "probing") {
		h.mode = mode
	}
}

// WithProtocolCompatibility wraps only an ordinary provider transport. It does
// not wrap credential tunnels or custom RoundTrippers, whose dialing and ALPN
// contracts it cannot safely infer. Call before publishing the client.
// The wrapper calls its selected transport exactly once, preserving that
// transport's existing behavior. An ambiguous HTTP/2 failure affects a later
// request, never replays the current sampling or tool execution.
func WithProtocolCompatibility(client *http.Client, identity RequestIdentity) *http.Client {
	base, ok := client.Transport.(*http.Transport)
	if !ok || base.DialTLS != nil || base.DialTLSContext != nil { //nolint:staticcheck // Legacy TLS dialers also own ALPN; do not wrap them.
		return client
	}
	h1 := base.Clone()
	h1.ForceAttemptHTTP2 = false
	h1.Protocols = new(http.Protocols)
	h1.Protocols.SetHTTP1(true)
	h1.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if h1.TLSClientConfig != nil {
		h1.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	// Resolve once per request and pin the same proxy for selection and dialing.
	// Do not mutate the base transport: it may already serve sibling streams.
	normal := base.Clone()
	normal.Proxy, h1.Proxy = selectedProtocolProxy, selectedProtocolProxy
	copy := *client
	copy.Transport = &protocolTransport{normal: normal, http1: h1, proxy: base.Proxy,
		scope: identity.Provider + "\x00" + identity.Protocol, hints: protocolCompatibilityHints}
	return &copy
}

type protocolProxyKey struct{}
type protocolProxy struct{ url *url.URL }

func selectedProtocolProxy(req *http.Request) (*url.URL, error) {
	p, _ := req.Context().Value(protocolProxyKey{}).(protocolProxy)
	return p.url, nil
}

type protocolTransport struct {
	normal, http1 *http.Transport
	proxy         func(*http.Request) (*url.URL, error)
	scope         string
	hints         *protocolHints
}

func (t *protocolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var proxy *url.URL
	var err error
	if t.proxy != nil {
		proxy, err = t.proxy(req)
		if err != nil {
			return nil, err
		}
	}
	proxyPath := "direct"
	if proxy != nil {
		proxyPath = proxy.String()
	}
	key := sha256.Sum256([]byte(t.scope + "\x00" + req.URL.String() + "\x00" + proxyPath))
	ctx := context.WithValue(req.Context(), protocolProxyKey{}, protocolProxy{proxy})
	req = req.WithContext(ctx)
	var hint *protocolHint
	if req.URL.Scheme == "https" && ctx.Err() == nil {
		hint = t.hints.selectHTTP1(key, time.Now())
	}
	transport := t.normal
	if hint != nil {
		transport = t.http1
		if observation, _ := ctx.Value(protocolObservationKey{}).(*requestObservationState); observation != nil {
			observation.update("protocol_selected", func(v *RequestObservation) { v.ProtocolFallbackReason = hint.reason })
		}
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.failed(ctx, key, hint, err)
		return resp, compatibilityFailure(hint, err)
	}
	if hint != nil {
		t.hints.settle(key, hint, "active")
	}
	resp.Body = &protocolResponseBody{ReadCloser: resp.Body, failed: func(err error) {
		t.failed(ctx, key, hint, err)
	}, hint: hint}
	return resp, nil
}

func (t *protocolTransport) failed(ctx context.Context, key [32]byte, hint *protocolHint, err error) {
	if ctx.Err() != nil && !errors.As(context.Cause(ctx), new(*RequestSendTimeoutError)) {
		if hint != nil {
			t.hints.settle(key, hint, "pending")
		}
		return
	}
	if hint != nil {
		t.hints.settle(key, hint, "disabled")
		return
	}
	switch code := HTTP2TransportCode(err); code {
	case "INTERNAL_ERROR", "PROTOCOL_ERROR", "HTTP_1_1_REQUIRED":
		t.hints.failedHTTP2(key, code, time.Now())
	}
}

func (t *protocolTransport) CloseIdleConnections() {
	t.normal.CloseIdleConnections()
	t.http1.CloseIdleConnections()
}

// A failed compatibility request is terminal even if its underlying cause is
// EOF/reset/send timeout. This prevents existing retry owners from stacking
// another resend or bouncing back to HTTP/2 within this request.
type protocolCompatibilityError struct{ error }

func (e *protocolCompatibilityError) Unwrap() error { return e.error }

func compatibilityFailure(hint *protocolHint, err error) error {
	if hint != nil && err != nil {
		return &protocolCompatibilityError{err}
	}
	return err
}

func protocolFailureIsTerminal(err error) bool {
	return HTTP2TransportCode(err) != "" || errors.As(err, new(*protocolCompatibilityError))
}

type protocolResponseBody struct {
	io.ReadCloser
	failed func(error)
	hint   *protocolHint
}

func (b *protocolResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		return n, err // normal stream completion is not a transport failure
	}
	if err != nil {
		b.failed(err)
	}
	return n, compatibilityFailure(b.hint, err)
}

func (b *protocolResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if err != nil {
		b.failed(err)
	}
	return compatibilityFailure(b.hint, err)
}
