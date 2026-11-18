package tests

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/tinypods-tunnel/internal/agent"
	"github.com/abhi1693/tinypods-tunnel/internal/relay"
)

type runningRelay struct {
	base    url.URL
	control string
	client  *http.Client
}

func newRelay(t *testing.T) runningRelay {
	t.Helper()
	public := httptest.NewUnstartedServer(nil)
	addr := public.Listener.Addr().(*net.TCPAddr)
	base := url.URL{Scheme: "http", Host: net.JoinHostPort("tunnel.test", strconv.Itoa(addr.Port))}
	r := relay.New(&base)
	public.Config.Handler = r.PublicHandler()
	public.Start()
	control := httptest.NewServer(r.AgentHandler())
	// Send wildcard test hostnames to the visitor listener without external DNS.
	dialer := &net.Dialer{}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr.String())
		}},
	}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		r.Close()
		control.Close()
		public.Close()
	})
	return runningRelay{base: base, control: "ws" + strings.TrimPrefix(control.URL, "http") + "/v1/connect", client: client}
}

func (r runningRelay) connect(t *testing.T, originURL, caFile string) (*url.URL, func()) {
	t.Helper()
	u, err := url.Parse(originURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	failed := make(chan error, 1)
	ready := make(chan string, 1)
	go func() {
		defer close(done)
		failed <- agent.Run(ctx, agent.Config{
			Scheme: u.Scheme, Port: port, RelayURL: r.control, CAFile: caFile,
		}, func(endpoint string) { ready <- endpoint })
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not exit after cancellation")
		}
	}
	t.Cleanup(stop)
	select {
	case advertised := <-ready:
		endpoint, err := url.Parse(advertised)
		if err != nil {
			t.Fatal(err)
		}
		label, ok := strings.CutSuffix(endpoint.Hostname(), "."+r.base.Hostname())
		if !ok || label == "" || strings.Contains(label, ".") ||
			endpoint.Scheme != r.base.Scheme || endpoint.Port() != r.base.Port() ||
			(endpoint.Path != "" && endpoint.Path != "/") {
			t.Fatalf("expected tunnel subdomain of %s, got %q", r.base.String(), advertised)
		}
		return endpoint, stop
	case err := <-failed:
		t.Fatalf("agent exited before registration: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("agent registration timed out")
	}
	return nil, nil
}

func TestConcurrentHTTPForwarding(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	payload := []byte{0, 1, 255, 128}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/slow" {
			close(started)
			select {
			case <-req.Context().Done():
			case <-release:
			}
			return
		}
		if req.Header.Get("X-Tinypod-Hop") != "" {
			t.Error("connection-specific request header reached origin")
		}
		w.Header().Set("X-Original-Target", req.RequestURI)
		w.Header().Set("X-Original-Host", req.Host)
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(http.StatusCreated)
		io.Copy(w, req.Body)
	}))
	t.Cleanup(origin.Close)
	r := newRelay(t)
	endpoint, _ := r.connect(t, origin.URL, "")
	client := r.client
	slowDone := make(chan error, 1)
	go func() {
		resp, err := client.Get(endpoint.JoinPath("slow").String())
		if resp != nil {
			resp.Body.Close()
		}
		slowDone <- err
	}()
	defer close(release)
	select {
	case <-started:
	case err := <-slowDone:
		t.Fatalf("slow request ended before reaching origin: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("slow request never reached origin")
	}
	target := "/callbacks/a%2Fb?b=2&a=1&a=3"
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(endpoint.String(), "/")+target, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "X-Tinypod-Hop")
	req.Header.Set("X-Tinypod-Hop", "remove me")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 || !bytes.Equal(got, payload) ||
		resp.Header.Get("X-Original-Target") != target ||
		resp.Header.Get("X-Original-Host") != req.URL.Host ||
		len(resp.Header.Values("Set-Cookie")) != 2 {
		t.Fatalf("unexpected tunneled response: status=%d headers=%v body=%x", resp.StatusCode, resp.Header, got)
	}
}

func TestTunnelIsolationAndLifecycle(t *testing.T) {
	newOrigin := func(name string) *httptest.Server {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, name)
		}))
		t.Cleanup(origin.Close)
		return origin
	}
	firstOrigin := newOrigin("first")
	secondOrigin := newOrigin("second")
	r := newRelay(t)
	first, stopFirst := r.connect(t, firstOrigin.URL, "")
	second, _ := r.connect(t, secondOrigin.URL, "")
	if first.Host == second.Host {
		t.Fatalf("connections received the same address %s", first)
	}

	get := func(t *testing.T, endpoint url.URL) (int, string) {
		t.Helper()
		resp, err := r.client.Get(endpoint.String())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	checkOrigin := func(t *testing.T, endpoint url.URL, want string) {
		t.Helper()
		status, body := get(t, endpoint)
		if status != http.StatusOK || body != want {
			t.Fatalf("GET %s returned %d %q, want 200 %q", endpoint.String(), status, body, want)
		}
	}
	t.Run("concurrent callers", func(t *testing.T) {
		for name, endpoint := range map[string]url.URL{"first": *first, "second": *second} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for range 8 {
					checkOrigin(t, endpoint, name)
				}
			})
		}
	})
	label := strings.TrimSuffix(first.Hostname(), "."+r.base.Hostname())
	for _, host := range []string{r.base.Host, "unknown." + r.base.Host, net.JoinHostPort(label+".other.test", r.base.Port())} {
		address := url.URL{Scheme: r.base.Scheme, Host: host}
		if status, _ := get(t, address); status != http.StatusNotFound {
			t.Fatalf("unknown host %q returned %d, want 404", host, status)
		}
	}
	upper := *second
	upper.Host = strings.ToUpper(upper.Host)
	checkOrigin(t, upper, "second")

	stopFirst()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _ := get(t, *first)
		if status == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnected endpoint still returns %d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	checkOrigin(t, *second, "second")
	reconnected, _ := r.connect(t, firstOrigin.URL, "")
	if reconnected.Host == first.Host {
		t.Fatalf("reconnect reused expired address %s", first)
	}
	checkOrigin(t, *reconnected, "first")
	if status, _ := get(t, *first); status != http.StatusNotFound {
		t.Fatalf("expired endpoint returned %d after reconnect, want 404", status)
	}
}

func TestHTTPSOriginTrust(t *testing.T) {
	cert, caFile := localCertificate(t)
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.TLS == nil {
			t.Error("expected TLS at origin")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	origin.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	origin.StartTLS()
	t.Cleanup(origin.Close)
	for _, tc := range []struct {
		name, ca string
		status   int
	}{
		{"trusted", caFile, 204},
		{"untrusted", "", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelay(t)
			endpoint, _ := r.connect(t, origin.URL, tc.ca)
			resp, err := r.client.Get(endpoint.String())
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func localCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "localhost.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, path
}
