package agent

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/http2"
)

// A webhook can finish its side effect and lose its connection before the
// response arrives. Even an empty keyed POST must reach the app only once.
func TestNoRedeliveryAfterOriginDisconnect(t *testing.T) {
	var deliveries atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		deliveries.Add(1)
		if r.Header.Get("Idempotency-Key") != "delivery-1" {
			t.Error("origin did not receive the idempotency key")
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer origin.Close()
	tr, err := newOriginTransport("", "http", x509.SystemCertPool)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.pooled.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	warm, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, warm.Body)
	warm.Body.Close()

	post, err := http.NewRequest(http.MethodPost, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	post.Header.Set("Idempotency-Key", "delivery-1")
	resp, err := client.Do(post)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected connection loss")
	}
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("origin received %d deliveries, want 1", got)
	}
}

func TestRunValidatesConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"scheme", Config{Scheme: "tcp", Port: 80}, "origin scheme"},
		{"low port", Config{Scheme: "http", Port: 0}, "origin port"},
		{"high port", Config{Scheme: "http", Port: 65536}, "origin port"},
		{"credentials in relay URL", Config{Scheme: "http", Port: 80, RelayURL: "ws://user@relay.test/"}, "TINYPOD_RELAY"},
		{"origin CA missing", Config{Scheme: "https", Port: 443, CAFile: filepath.Join(t.TempDir(), "missing")}, "read origin CA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(t.Context(), tc.cfg, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run returned %v, want %q", err, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Run(ctx, Config{Scheme: "http", Port: 80}, nil); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "127.0.0.1:8081") {
		t.Fatalf("default relay: got %v", err)
	}
}

func TestAgentRejectsRelayRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("followed relay redirect") }))
	defer target.Close()
	relay := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer relay.Close()
	err := Run(t.Context(), Config{Scheme: "http", Port: 80, RelayURL: "ws" + strings.TrimPrefix(relay.URL, "http")}, nil)
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect: got %v", err)
	}
}

func TestAgentDetectsIncompatibleOrDisconnectedRelay(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protocols []string
		want      string
	}{
		{"missing subprotocol", nil, "relay did not negotiate"},
		{"lost session", []string{"tinypod.h2.v1"}, "relay disconnected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: tc.protocols})
				if err != nil {
					t.Error(err)
					return
				}
				ws.CloseNow()
			}))
			defer control.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := Run(ctx, Config{Scheme: "http", Port: 80, RelayURL: "ws" + strings.TrimPrefix(control.URL, "http")}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run returned %v, want %q", err, tc.want)
			}
		})
	}
}

// A real WebSocket carries the HTTP/2 readiness messages in these tests.
// The short timeout is local to this session.
func openAgentSession(t *testing.T, timeout time.Duration, onReady func(string)) (*websocket.Conn, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer ws.CloseNow()
		done <- serveSession(t.Context(), ws, http.NotFoundHandler(), onReady, timeout)
	}))
	t.Cleanup(server.Close)
	ws, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return ws, done
}

func TestSessionRequiresTimelyReadiness(t *testing.T) {
	_, done := openAgentSession(t, 30*time.Millisecond, nil)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not confirm readiness") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("silent relay did not time out")
	}
}

func TestSessionAnnouncesOnlyFirstValidEndpoint(t *testing.T) {
	ready := make(chan string, 3)
	ws, _ := openAgentSession(t, 3*time.Second, func(u string) { ready <- u })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cc, err := (&http2.Transport{AllowHTTP: true}).NewClientConn(websocket.NetConn(ctx, ws, websocket.MessageBinary))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	request := func(method, endpoint string, wantStatus int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://control.test/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tinypod-URL", endpoint)
		res, err := cc.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != wantStatus {
			t.Fatalf("%s %s: got %d, want %d", method, endpoint, res.StatusCode, wantStatus)
		}
	}
	request("TINYPOD-READY", "https://user@relay.test", 400)
	request("TINYPOD-READY", "https://first.test", 204)
	request("TINYPOD-READY", "https://second.test", 204)
	request(http.MethodGet, "", 404)
	if len(ready) != 1 {
		t.Fatalf("received %d readiness callbacks, want 1", len(ready))
	}
	if got := <-ready; got != "https://first.test" {
		t.Fatalf("announced %q", got)
	}
}

func TestOriginCertificatePool(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "local.pem")
	if err := os.WriteFile(pemPath, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	brokenStore := errors.New("root store unavailable")
	_, err := newOriginTransport(pemPath, "https", func() (*x509.CertPool, error) { return nil, brokenStore })
	if !errors.Is(err, brokenStore) {
		t.Fatalf("system certificate failure: %v", err)
	}
	if _, err := newOriginTransport(pemPath, "https", x509.SystemCertPool); err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Fatalf("invalid PEM: %v", err)
	}

	origin := httptest.NewTLSServer(nil)
	defer origin.Close()
	cert := origin.Certificate()
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	tr, err := newOriginTransport(pemPath, "https", x509.SystemCertPool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: tr.pooled.TLSClientConfig.RootCAs}); err != nil {
		t.Fatalf("configured CA was not trusted: %v", err)
	}
}

func TestOriginProxyForwardsHeadersAndHandlesUnavailableOrigin(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "visitor.test" || r.URL.RawQuery != "key=a%2Fb" || r.Header.Get("X-Forwarded-For") != "192.0.2.1" || r.Header.Get("X-Forwarded-Proto") != "https" || r.Header.Get("X-Forwarded-Host") != "visitor.test" {
			t.Errorf("unexpected origin request: host %s, query %s, headers %v", r.Host, r.URL.RawQuery, r.Header)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer origin.Close()
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := newOriginTransport("", "http", x509.SystemCertPool)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.pooled.CloseIdleConnections()
	proxy := originProxy(u, tr)
	req := httptest.NewRequest(http.MethodPost, "https://visitor.test/webhook?key=a%2Fb", nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "visitor.test")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("origin status %d", w.Code)
	}
	origin.Close()
	w = httptest.NewRecorder()
	proxy.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "local origin unavailable") {
		t.Fatalf("failed origin: %d %q", w.Code, w.Body.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w = httptest.NewRecorder()
	proxy.ServeHTTP(w, req.WithContext(ctx))
	if w.Body.Len() != 0 || len(w.Header()) != 0 {
		t.Fatalf("wrote after visitor cancellation: %v %s", w.Header(), w.Body)
	}
}
