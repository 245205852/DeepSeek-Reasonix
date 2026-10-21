package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func compatibilityTestClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	c := WithProtocolCompatibility(server.Client(), RequestIdentity{Provider: t.Name(), Protocol: "openai"})
	tr := c.Transport.(*protocolTransport)
	tr.hints = freshProtocolHints()
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func compatibilityPost(ctx context.Context, client *http.Client, endpoint string) (*http.Response, error) {
	return SendWithRetry(ctx, client, SendOptions{Provider: "test", Protocol: "openai"}, func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"messages":[{"role":"user","content":"unchanged"}]}`))
	})
}

func TestProtocolCompatibilityUsesOnlySubsequentRequests(t *testing.T) {
	for _, bodyFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers-no-observer", true: "partial-stream-observed"}[bodyFailure], func(t *testing.T) {
			var requests, proxies atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				b, err := io.ReadAll(r.Body)
				if err != nil || string(b) != `{"messages":[{"role":"user","content":"unchanged"}]}` {
					t.Error("request body changed")
				}
				if r.ProtoMajor == 2 {
					if bodyFailure {
						_, _ = io.WriteString(w, "data: partial\n\n")
						w.(http.Flusher).Flush()
					}
					panic(http.ErrAbortHandler) // real HTTP/2 RST_STREAM INTERNAL_ERROR
				}
				_, _ = io.WriteString(w, "data: complete\n\n")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			client := compatibilityTestClient(t, server)
			client.Transport.(*protocolTransport).proxy = func(*http.Request) (*url.URL, error) {
				proxies.Add(1)
				return nil, nil
			}
			var mu sync.Mutex
			var last RequestObservation
			baseCtx := t.Context()
			if bodyFailure {
				baseCtx = WithRequestObserver(baseCtx, func(v RequestObservation) { mu.Lock(); last = v; mu.Unlock() })
			}
			for attempt := 1; attempt <= 3; attempt++ {
				ctx := WithRequestAttemptCounter(baseCtx)
				resp, err := compatibilityPost(ctx, client, server.URL+"/chat")
				if resp != nil {
					_, err = io.ReadAll(resp.Body)
					resp.Body.Close()
				}
				if attempt == 1 {
					if HTTP2TransportCode(err) != "INTERNAL_ERROR" {
						t.Fatalf("missing typed HTTP/2 failure: %v", err)
					}
				} else if err != nil || resp.ProtoMajor != 1 {
					t.Fatalf("subsequent request did not use HTTP/1.1: resp=%v err=%v", resp, err)
				}
				if requests.Load() != int32(attempt) || proxies.Load() != int32(attempt) || RequestAttemptCount(ctx) != 1 {
					t.Fatalf("replayed request or resolved proxy twice: requests=%d proxies=%d count=%d", requests.Load(), proxies.Load(), RequestAttemptCount(ctx))
				}
			}
			mu.Lock()
			if bodyFailure && (last.ProtocolFallbackReason != "INTERNAL_ERROR" || last.HTTPProtocol != "HTTP/1.1" || last.TransportCode != "") {
				t.Errorf("lost compatibility evidence: %+v", last)
			}
			mu.Unlock()
			// Another endpoint remains HTTP/2, even with the same client and provider.
			resp, err := compatibilityPost(t.Context(), client, server.URL+"/other")
			if resp != nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if HTTP2TransportCode(err) != "INTERNAL_ERROR" {
				t.Fatalf("compatibility escaped endpoint scope: %v", err)
			}
		})
	}
}

func TestProtocolCompatibilityFailureDoesNotStackResends(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(map[bool]string{false: "eof", true: "send-timeout"}[stall], func(t *testing.T) {
			var requests atomic.Int32
			release := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.ProtoMajor == 2 {
					panic(http.ErrAbortHandler)
				}
				if stall {
					<-release
					return
				}
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			defer close(release)
			server.Client().Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err == nil {
					_ = conn.(*net.TCPConn).SetWriteBuffer(1024)
				}
				return conn, err
			}
			client := compatibilityTestClient(t, server)
			_, err := compatibilityPost(t.Context(), client, server.URL)
			if HTTP2TransportCode(err) != "INTERNAL_ERROR" {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var last RequestObservation
			ctx := WithRequestAttemptCounter(WithRequestObserver(t.Context(), func(v RequestObservation) {
				mu.Lock()
				last = v
				mu.Unlock()
			}))
			_, err = sendWithResends(ctx, client, SendOptions{}, func(ctx context.Context) (*http.Request, error) {
				body := "small"
				if stall {
					body = strings.Repeat("x", 3_248_572)
				}
				return http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(body))
			}, 500*time.Millisecond, 1)
			if !errors.As(err, new(*protocolCompatibilityError)) || ClassifyRecovery(err).Retryable {
				t.Fatalf("lost terminal compatibility marker: %v", err)
			}
			if stall && !errors.As(err, new(*RequestSendTimeoutError)) {
				t.Fatalf("lost timeout cause: %v", err)
			}
			if requests.Load() != 2 || RequestAttemptCount(ctx) != 1 {
				t.Fatalf("stacked retry: server=%d count=%d", requests.Load(), RequestAttemptCount(ctx))
			}
			mu.Lock()
			evidence := last
			mu.Unlock()
			if evidence.ProtocolFallbackReason != "INTERNAL_ERROR" || evidence.TransportCode != "" || evidence.SendTimeoutMs != 500 || evidence.Phase != "request_error" {
				t.Fatalf("lost compatibility/transport evidence: %+v", evidence)
			}
			if stall {
				if evidence.ErrorCode != "request_send_timeout" || evidence.ErrorPhase != "request_write" || evidence.TimeoutSource != "send_guard" || evidence.SendElapsedMs < 500 {
					t.Fatalf("lost guarded send failure evidence: %+v", evidence)
				}
			} else if evidence.ErrorCode != "eof" {
				t.Fatalf("lost EOF cause under compatibility marker: %+v", evidence)
			}
			// The failed H1 probe disables compatibility until the original expiry.
			_, err = compatibilityPost(t.Context(), client, server.URL)
			if HTTP2TransportCode(err) != "INTERNAL_ERROR" || requests.Load() != 3 {
				t.Fatalf("failed compatibility looped: requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}
