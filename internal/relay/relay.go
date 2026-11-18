// Package relay routes each tunnel hostname to its outbound agent session.
package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/http2"
)

const (
	subprotocol    = "tinypod.h2.v1"
	maxRequestBody = 1 << 20
	maxConcurrent  = 128
	maxTunnels     = 128
	requestTimeout = 30 * time.Second
)

type Relay struct {
	baseURL url.URL
	ctx     context.Context
	stop    context.CancelFunc
	slots   chan struct{}

	mu      sync.RWMutex
	tunnels map[string]*httputil.ReverseProxy
}

func New(baseURL *url.URL) *Relay {
	ctx, stop := context.WithCancel(context.Background())
	return &Relay{
		baseURL: *baseURL,
		ctx:     ctx,
		stop:    stop,
		slots:   make(chan struct{}, maxConcurrent),
		tunnels: make(map[string]*httputil.ReverseProxy),
	}
}

func (r *Relay) Close() { r.stop() }

func (r *Relay) AgentHandler() http.Handler  { return http.HandlerFunc(r.connect) }
func (r *Relay) PublicHandler() http.Handler { return http.HandlerFunc(r.forward) }

// Reserve before upgrading so pending handshakes count towards the session limit.
// A nil proxy cannot receive traffic until the agent's HTTP/2 connection is ready.
func (r *Relay) reserve() (url.URL, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil || len(r.tunnels) >= maxTunnels {
		return url.URL{}, false
	}
	for {
		endpoint := r.baseURL
		// Keep the label at 26 base32 characters even if Go increases Text's length.
		endpoint.Host = strings.ToLower(rand.Text()[:26]) + "." + r.baseURL.Host
		host := canonicalHost(endpoint.Host)
		if _, exists := r.tunnels[host]; !exists {
			r.tunnels[host] = nil
			return endpoint, true
		}
	}
}

func canonicalHost(authority string) string {
	u := url.URL{Host: authority}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

func (r *Relay) connect(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/v1/connect" {
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodGet || req.Header.Get("Origin") != "" ||
		!slices.ContainsFunc(strings.Split(req.Header.Get("Sec-WebSocket-Protocol"), ","), func(p string) bool {
			return strings.TrimSpace(p) == subprotocol
		}) {
		http.Error(w, "expected a TinyPod agent", http.StatusBadRequest)
		return
	}

	endpoint, ok := r.reserve()
	if !ok {
		http.Error(w, "tunnel capacity unavailable", http.StatusServiceUnavailable)
		return
	}
	host := canonicalHost(endpoint.Host)
	defer func() {
		r.mu.Lock()
		delete(r.tunnels, host)
		r.mu.Unlock()
	}()

	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{subprotocol}})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	carrier := &sessionConn{Conn: websocket.NetConn(ctx, ws, websocket.MessageBinary), cancel: cancel}
	ws.SetReadLimit(1 << 20)
	r.serveSession(ctx, carrier, endpoint)
}

func (r *Relay) serveSession(ctx context.Context, carrier net.Conn, endpoint url.URL) {
	tr := &http2.Transport{
		AllowHTTP: true, DisableCompression: true,
		MaxHeaderListSize: 32 << 10, ReadIdleTimeout: 15 * time.Second,
		PingTimeout: 5 * time.Second, WriteByteTimeout: 10 * time.Second,
	}
	cc, err := tr.NewClientConn(carrier)
	if err != nil {
		return
	}
	defer cc.Close()
	setup, finish := context.WithTimeout(ctx, 5*time.Second)
	defer finish()
	if err := cc.Ping(setup); err != nil {
		return
	}

	// Publish before notifying the CLI so its printed URL is immediately usable.
	r.mu.Lock()
	r.tunnels[canonicalHost(endpoint.Host)] = newProxy(cc, endpoint.Scheme)
	r.mu.Unlock()
	ready := (&http.Request{
		Method: "TINYPOD-READY",
		URL:    &url.URL{Scheme: "http", Host: "control.tinypod.invalid", Path: "/"},
		Header: make(http.Header),
	}).WithContext(setup)
	ready.Header.Set("X-Tinypod-URL", endpoint.String())
	res, err := cc.RoundTrip(ready)
	if err != nil {
		return
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return
	}
	finish()
	<-ctx.Done()
}

// HTTP/2 closes its underlying connection when the peer disappears.
type sessionConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *sessionConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func newProxy(cc *http2.ClientConn, scheme string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: cc, // A single round trip. Never redial and replay on another session.
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme = scheme
			p.Out.URL.Host = p.In.Host
			p.Out.URL.RawQuery = p.In.URL.RawQuery
			p.Out.Header.Del("X-Real-Ip")
			p.SetXForwarded()
			p.Out.Header.Set("X-Forwarded-Proto", scheme)
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			http.Error(w, http.StatusText(status), status)
		},
	}
}

func (r *Relay) forward(w http.ResponseWriter, req *http.Request) {
	if req.URL.IsAbs() || req.URL.Host != "" || req.URL.Opaque != "" || !strings.HasPrefix(req.RequestURI, "/") {
		http.Error(w, "expected an origin-form request target", http.StatusBadRequest)
		return
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
		return
	}
	if req.Header.Get("Upgrade") != "" {
		http.Error(w, "public protocol upgrades are unsupported", http.StatusBadRequest)
		return
	}
	r.mu.RLock()
	proxy := r.tunnels[canonicalHost(req.Host)]
	r.mu.RUnlock()
	if proxy == nil {
		http.NotFound(w, req)
		return
	}
	if r.ctx.Err() != nil {
		http.Error(w, "relay stopping", http.StatusServiceUnavailable)
		return
	}
	if req.ContentLength > maxRequestBody {
		http.Error(w, "request body exceeds 1 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		http.Error(w, "relay at capacity", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), requestTimeout)
	defer cancel()

	// Validate unknown-length uploads before handing any bytes to the origin.
	if req.ContentLength < 0 {
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxRequestBody))
		if err != nil {
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				http.Error(w, "request body exceeds 1 MiB", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "could not read request body", http.StatusBadRequest)
			}
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.TransferEncoding = nil
	}

	proxy.ServeHTTP(w, req.WithContext(ctx))
}
