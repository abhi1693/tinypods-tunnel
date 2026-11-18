package relay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func registerTransport(r *Relay, transport roundTripFunc) string {
	endpoint, _ := r.reserve()
	proxy := newProxy(nil, endpoint.Scheme)
	proxy.Transport = transport
	r.tunnels[endpoint.Host] = proxy
	return endpoint.Host
}

func TestPublicRequestValidation(t *testing.T) {
	r := testRelay(t)
	host := registerTransport(r, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached the agent")
		return nil, nil
	})
	for _, tc := range []struct {
		name, method, target, upgrade string
		size                          int64
		want                          int
	}{
		{"absolute target", "GET", "https://other.test/app", "", 0, 400},
		{"unsupported method", "TRACE", "/", "", 0, 405},
		{"upgrade", "GET", "/", "websocket", 0, 400},
		{"declared oversized upload", "POST", "/", "", maxRequestBody + 1, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.Host = host
			req.ContentLength = tc.size
			req.Header.Set("Upgrade", tc.upgrade)
			w := httptest.NewRecorder()
			r.PublicHandler().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
			if tc.want == 405 && w.Header().Get("Allow") != "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS" {
				t.Errorf("unexpected allowed methods %q", w.Header().Get("Allow"))
			}
		})
	}
	r.Close()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = host
	w := httptest.NewRecorder()
	r.PublicHandler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopped relay returned %d", w.Code)
	}
}

func TestUnknownLengthUploads(t *testing.T) {
	data := bytes.Repeat([]byte("x"), maxRequestBody)
	for _, tc := range []struct {
		name      string
		body      io.Reader
		want      int
		wantCalls int
	}{
		{"exact limit", bytes.NewReader(data), 201, 1},
		{"over limit", io.MultiReader(bytes.NewReader(data), strings.NewReader("x")), 413, 0},
		{"interrupted", iotest.ErrReader(io.ErrUnexpectedEOF), 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testRelay(t)
			calls := 0
			host := registerTransport(r, func(req *http.Request) (*http.Response, error) {
				calls++
				got, err := io.ReadAll(req.Body)
				if err != nil || !bytes.Equal(got, data) || req.ContentLength != int64(len(data)) || len(req.TransferEncoding) != 0 {
					t.Errorf("forwarded upload length %d, declared %d, encodings %v, read error %v", len(got), req.ContentLength, req.TransferEncoding, err)
				}
				return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody}, nil
			})
			req := httptest.NewRequest("POST", "/webhook", tc.body)
			req.Host = host
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}
			w := httptest.NewRecorder()
			r.PublicHandler().ServeHTTP(w, req)
			if w.Code != tc.want || calls != tc.wantCalls {
				t.Fatalf("status %d, calls %d; want %d, %d", w.Code, calls, tc.want, tc.wantCalls)
			}
		})
	}
}

func TestUpstreamFailuresReturnGatewayStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{io.ErrUnexpectedEOF, http.StatusBadGateway},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
	} {
		r := testRelay(t)
		host := registerTransport(r, func(*http.Request) (*http.Response, error) { return nil, tc.err })
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = host
		w := httptest.NewRecorder()
		r.PublicHandler().ServeHTTP(w, req)
		if w.Code != tc.want || strings.TrimSpace(w.Body.String()) != http.StatusText(tc.want) {
			t.Fatalf("for %v got %d %q, want %d", tc.err, w.Code, w.Body, tc.want)
		}
	}
}

func TestProxyReplacesVisitorForwardingHeaders(t *testing.T) {
	r := testRelay(t)
	host := registerTransport(r, func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-Forwarded-For") != "192.0.2.4" ||
			req.Header.Get("X-Forwarded-Proto") != "https" ||
			req.Header.Get("X-Forwarded-Host") != req.Host ||
			req.Header.Get("X-Real-IP") != "" || req.Header.Get("Forwarded") != "" {
			t.Errorf("forwarded headers %v for host %q", req.Header, req.Host)
		}
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	})
	req := httptest.NewRequest("GET", "/callback", nil)
	req.Host = host
	req.RemoteAddr = "192.0.2.4:1500"
	for _, key := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-IP", "Forwarded"} {
		req.Header.Set(key, "visitor-supplied")
	}
	w := httptest.NewRecorder()
	r.PublicHandler().ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("proxy returned %d", w.Code)
	}
}

func TestForwardingCapacityRecoversAfterRequestsFinish(t *testing.T) {
	r := testRelay(t)
	entered := make(chan struct{}, maxConcurrent)
	release, finish := context.WithCancel(t.Context())
	defer finish()
	host := registerTransport(r, func(req *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release.Done():
			return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
		}
	})
	call := func() int {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = host
		w := httptest.NewRecorder()
		r.PublicHandler().ServeHTTP(w, req)
		return w.Code
	}
	completed := make(chan int, maxConcurrent)
	for range maxConcurrent {
		go func() { completed <- call() }()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range maxConcurrent {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("failed to fill relay to its published capacity")
		}
	}
	if status := call(); status != http.StatusServiceUnavailable {
		t.Fatalf("saturated relay returned %d", status)
	}
	finish()
	for range maxConcurrent {
		select {
		case code := <-completed:
			if code != 204 {
				t.Fatalf("in-flight request returned %d", code)
			}
		case <-ctx.Done():
			t.Fatal("in-flight request failed to finish")
		}
	}
	if status := call(); status != 204 {
		t.Fatalf("relay returned %d after freeing capacity", status)
	}
}
