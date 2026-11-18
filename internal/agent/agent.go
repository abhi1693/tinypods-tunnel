// Package agent connects a single loopback HTTP service to the relay.
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/http2"
)

const DefaultRelay = "ws://127.0.0.1:8081/v1/connect"

type Config struct {
	Scheme   string
	Port     int
	RelayURL string
	CAFile   string
}

// Run forwards requests until the caller cancels ctx or the relay disconnects.
// onReady receives the visitor URL after the relay has installed the session.
func Run(ctx context.Context, cfg Config, onReady func(string)) error {
	if cfg.Scheme != "http" && cfg.Scheme != "https" {
		return errors.New("origin scheme must be http or https")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("origin port must be between 1 and 65535")
	}
	if cfg.RelayURL == "" {
		cfg.RelayURL = DefaultRelay
	}
	relay, err := url.Parse(cfg.RelayURL)
	if err != nil || relay.Hostname() == "" || (relay.Scheme != "ws" && relay.Scheme != "wss") || relay.User != nil || relay.Fragment != "" {
		return errors.New("TINYPOD_RELAY must be a ws:// or wss:// URL")
	}

	transport, err := newOriginTransport(cfg.CAFile, cfg.Scheme, x509.SystemCertPool)
	if err != nil {
		return err
	}
	defer transport.pooled.CloseIdleConnections()
	origin := &url.URL{Scheme: cfg.Scheme, Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))}
	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	ws, _, err := websocket.Dial(dialCtx, relay.String(), &websocket.DialOptions{
		Subprotocols: []string{"tinypod.h2.v1"},
		HTTPClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	})
	cancelDial()
	if err != nil {
		return fmt.Errorf("connect to relay: %w", err)
	}
	defer ws.CloseNow()
	if ws.Subprotocol() != "tinypod.h2.v1" {
		return errors.New("relay did not negotiate tinypod.h2.v1")
	}

	return serveSession(ctx, ws, originProxy(origin, transport), onReady, 10*time.Second)
}

func originProxy(origin *url.URL, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(req *httputil.ProxyRequest) {
			// The visitor controls the request target, never the dial address.
			req.Out.URL.Scheme = origin.Scheme
			req.Out.URL.Host = origin.Host
			req.Out.URL.RawQuery = req.In.URL.RawQuery
			req.Out.Host = req.In.Host
			// The relay constructs these headers at its public listener.
			for _, name := range []string{"X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-For"} {
				if value := req.In.Header.Get(name); value != "" {
					req.Out.Header.Set(name, value)
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			log.Printf("local %s origin: %v", origin.Scheme, err)
			http.Error(w, "local origin unavailable", http.StatusBadGateway)
		},
	}
}

func serveSession(ctx context.Context, ws *websocket.Conn, proxy http.Handler, onReady func(string), readyTimeout time.Duration) error {
	session, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	startup := time.AfterFunc(readyTimeout, func() {
		cancel(fmt.Errorf("relay did not confirm readiness within %s", readyTimeout))
	})
	defer startup.Stop()

	conn := websocket.NetConn(session, ws, websocket.MessageBinary)
	ws.SetReadLimit(1 << 20)
	var ready sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "TINYPOD-READY" {
			public, err := url.Parse(r.Header.Get("X-Tinypod-URL"))
			if err != nil || public.Hostname() == "" || (public.Scheme != "http" && public.Scheme != "https") || public.User != nil {
				http.Error(w, "invalid relay URL", http.StatusBadRequest)
				return
			}
			ready.Do(func() {
				startup.Stop()
				if onReady != nil {
					onReady(public.String())
				}
			})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	// The relay opens HTTP/2 streams over the connection the CLI dialed.
	h2 := &http2.Server{MaxConcurrentStreams: 128}
	h2.ServeConn(conn, &http2.ServeConnOpts{
		Context:    session,
		Handler:    handler,
		BaseConfig: &http.Server{MaxHeaderBytes: 32 << 10},
	})
	if cause := context.Cause(session); cause != nil {
		return cause
	}
	return errors.New("relay disconnected")
}

type originTransport struct {
	pooled *http.Transport
	fresh  *http.Transport
}

func newOriginTransport(caFile, scheme string, systemRoots func() (*x509.CertPool, error)) (*originTransport, error) {
	tlsConfig := &tls.Config{ServerName: "localhost", MinVersion: tls.VersionTLS12}
	if caFile != "" && scheme == "https" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read origin CA: %w", err)
		}
		roots, err := systemRoots()
		if err != nil {
			return nil, fmt.Errorf("load system root certificates: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("origin CA file contains no PEM certificates")
		}
		tlsConfig.RootCAs = roots
	}
	pooled := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression:     true,
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    5 * time.Second,
		IdleConnTimeout:        90 * time.Second,
		MaxIdleConnsPerHost:    128,
		MaxConnsPerHost:        128,
		MaxResponseHeaderBytes: 32 << 10,
	}
	fresh := pooled.Clone()
	fresh.DisableKeepAlives = true
	return &originTransport{pooled: pooled, fresh: fresh}, nil
}

func (t *originTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// net/http may replay an empty POST marked with an idempotency key after
	// a reused connection breaks. Preserve the key and use a fresh connection.
	keyed := req.Header.Values("Idempotency-Key") != nil || req.Header.Values("X-Idempotency-Key") != nil
	if keyed && (req.Body == nil || req.Body == http.NoBody) {
		return t.fresh.RoundTrip(req)
	}
	return t.pooled.RoundTrip(req)
}
