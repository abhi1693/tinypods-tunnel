package relay

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/http2"
)

func testRelay(t *testing.T) *Relay {
	t.Helper()
	r := New(&url.URL{Scheme: "https", Host: "tunnels.test"})
	t.Cleanup(r.Close)
	return r
}

func TestAgentHandshakeRejectsInvalidRequests(t *testing.T) {
	r := testRelay(t)
	for _, tc := range []struct {
		name, method, path, origin, protocol string
		want                                 int
	}{
		{"wrong path", "GET", "/", "", subprotocol, 404},
		{"wrong method", "POST", "/v1/connect", "", subprotocol, 400},
		{"browser origin", "GET", "/v1/connect", "https://browser.test", subprotocol, 400},
		{"wrong protocol", "GET", "/v1/connect", "", "other", 400},
		{"missing upgrade", "GET", "/v1/connect", "", "other, " + subprotocol, 426},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-WebSocket-Protocol", tc.protocol)
			w := httptest.NewRecorder()
			r.AgentHandler().ServeHTTP(w, req)
			if w.Code != tc.want || len(r.tunnels) != 0 {
				t.Fatalf("status %d, reservations %d; want %d, 0", w.Code, len(r.tunnels), tc.want)
			}
		})
	}
}

func TestPendingSessionsConsumeTunnelCapacity(t *testing.T) {
	r := testRelay(t)
	for range maxTunnels {
		addr, ok := r.reserve()
		if !ok {
			t.Fatalf("reservation failed with %d tunnels", len(r.tunnels))
		}
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = addr.Host
		w := httptest.NewRecorder()
		r.PublicHandler().ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("unconnected host returned %d", w.Code)
		}
	}
	req := httptest.NewRequest("GET", "/v1/connect", nil)
	req.Header.Set("Sec-WebSocket-Protocol", subprotocol)
	w := httptest.NewRecorder()
	r.AgentHandler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || len(r.tunnels) != maxTunnels {
		t.Fatalf("excess agent got %d, %d reservations", w.Code, len(r.tunnels))
	}
}

func TestPeerDisconnectDuringSetup(t *testing.T) {
	for _, stage := range []string{"preface", "ping"} {
		t.Run(stage, func(t *testing.T) {
			r := testRelay(t)
			endpoint, _ := r.reserve()
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if stage == "preface" {
				peer.Close()
				r.serveSession(ctx, client, endpoint)
			} else {
				done := make(chan struct{})
				go func() {
					defer close(done)
					r.serveSession(ctx, client, endpoint)
				}()
				deadline, _ := ctx.Deadline()
				peer.SetDeadline(deadline)
				frame := http2.NewFramer(nil, peer)
				preface := make([]byte, len(http2.ClientPreface))
				if _, err := io.ReadFull(peer, preface); err != nil || string(preface) != http2.ClientPreface {
					t.Fatalf("client preface %q, error %v", preface, err)
				}
				for {
					f, err := frame.ReadFrame()
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := f.(*http2.PingFrame); ok {
						break
					}
				}
				peer.Close()
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("session failed to terminate after peer disconnect")
				}
			}
			if r.tunnels[endpoint.Host] != nil {
				t.Fatal("relay published a tunnel without a completed handshake")
			}
		})
	}
}

func TestReadyHandshakeAndCleanup(t *testing.T) {
	for _, reply := range []string{"accept", "reject", "reset"} {
		t.Run(reply, func(t *testing.T) {
			r := testRelay(t)
			done := make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				defer close(done)
				r.AgentHandler().ServeHTTP(w, req)
			}))
			defer s.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ws, _, err := websocket.Dial(ctx, s.URL+"/v1/connect", &websocket.DialOptions{Subprotocols: []string{subprotocol}})
			if err != nil {
				t.Fatal(err)
			}
			defer ws.CloseNow()
			ready := make(chan string, 1)
			h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != "TINYPOD-READY" {
					w.WriteHeader(http.StatusCreated)
					return
				}
				if req.Host != "control.tinypod.invalid" || req.URL.Path != "/" {
					t.Errorf("unexpected ready target %s%s", req.Host, req.URL.Path)
				}
				ready <- req.Header.Get("X-Tinypod-URL")
				switch reply {
				case "accept":
					w.WriteHeader(http.StatusNoContent)
				case "reject":
					w.WriteHeader(http.StatusForbidden)
				case "reset":
					panic(http.ErrAbortHandler)
				}
			})
			conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
			go new(http2.Server).ServeConn(conn, &http2.ServeConnOpts{Context: ctx, Handler: h})
			var endpoint *url.URL
			select {
			case address := <-ready:
				endpoint, err = url.Parse(address)
				if err != nil || endpoint.Scheme != "https" || !strings.HasSuffix(endpoint.Host, ".tunnels.test") {
					t.Fatalf("ready address %q, error %v", address, err)
				}
			case <-ctx.Done():
				t.Fatal("agent did not receive ready request")
			}
			req := httptest.NewRequestWithContext(ctx, "GET", "/app", nil)
			req.Host = endpoint.Host
			if reply == "accept" {
				w := httptest.NewRecorder()
				r.PublicHandler().ServeHTTP(w, req)
				if w.Code != http.StatusCreated {
					t.Fatalf("new tunnel returned %d", w.Code)
				}
				ws.CloseNow()
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("relay did not release the session")
			}
			w := httptest.NewRecorder()
			r.PublicHandler().ServeHTTP(w, req)
			if w.Code != http.StatusNotFound || len(r.tunnels) != 0 {
				t.Fatalf("after disconnect got %d, %d remaining reservations", w.Code, len(r.tunnels))
			}
		})
	}
}
