package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Run the relay itself in a child process to check its exit status and diagnostics.
func TestMain(m *testing.M) {
	if os.Getenv("TINYPOD_TEST_RELAY_COMMAND") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func listener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestRelayCommandReportsStartupErrors(t *testing.T) {
	occupied := listener(t).Addr().String()
	for _, tc := range []struct {
		name, visitors, agents, base, diagnostic string
	}{
		{"visitor port in use", occupied, "127.0.0.1:0", "", occupied},
		{"agent port in use", "127.0.0.1:0", occupied, "", occupied},
		{"invalid public origin", "127.0.0.1:0", "127.0.0.1:0", "ftp://localhost", "-public-url must be an HTTP(S) origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-listen", tc.visitors, "-agent-listen", tc.agents, "-public-url", tc.base)
			cmd.Env = append(os.Environ(), "TINYPOD_TEST_RELAY_COMMAND=1")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), tc.diagnostic) {
				t.Fatalf("relay exited with %v, output %q; want status 1 and %q", err, out, tc.diagnostic)
			}
		})
	}
}

func TestRelayServesBothListenersUntilCanceled(t *testing.T) {
	visitors, agents := listener(t), listener(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, visitors, agents, &url.URL{Scheme: "http", Host: "localhost"}) }()
	request := func(addr string, status int) {
		t.Helper()
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + addr + "/v1/connect")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != status {
			t.Errorf("%s returned HTTP %d, want %d", addr, resp.StatusCode, status)
		}
	}
	request(visitors.Addr().String(), http.StatusNotFound)
	request(agents.Addr().String(), http.StatusBadRequest)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not stop after cancellation")
	}
	for _, ln := range []net.Listener{visitors, agents} {
		assertListenerClosed(t, ln)
	}
}

func TestRunCanceledDuringStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := run(ctx, "127.0.0.1:0", "127.0.0.1:0", ""); err != nil {
		t.Fatalf("canceled startup returned %v", err)
	}
}

func TestListenerFailureStopsPeer(t *testing.T) {
	visitors, agents := listener(t), listener(t)
	visitors.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := serve(ctx, visitors, agents, &url.URL{Scheme: "http", Host: "localhost"})
	if !errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
		t.Fatalf("serve returned %v with context %v; want failed listener to stop the relay", err, ctx.Err())
	}
	assertListenerClosed(t, agents)
}

func assertListenerClosed(t *testing.T, ln net.Listener) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err == nil {
		conn.Close()
		t.Errorf("listener %s accepted a new connection after shutdown", ln.Addr())
	}
}

func TestTunnelBase(t *testing.T) {
	listener := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30123}
	for _, tc := range []struct{ input, want string }{
		{"", "http://localhost:30123"},
		{"http://127.0.0.1:8080", "http://localhost:8080"},
		{"http://[::1]:8080", "http://localhost:8080"},
		{"https://Tunnels.Example.org:8443/", "https://tunnels.example.org:8443"},
		{"http://192.0.2.10", ""},
		{"http://*.example.org", ""},
		{"https://example.org/visitor", ""},
		{"https://example.org:90000", ""},
		{"http://" + strings.Repeat(strings.Repeat("a", 56)+".", 3) + strings.Repeat("a", 56), ""},
	} {
		got, err := tunnelBase(tc.input, listener)
		if tc.want == "" {
			if err == nil {
				t.Errorf("accepted invalid base %q", tc.input)
			}
			continue
		}
		if err != nil || got.String() != tc.want {
			t.Errorf("tunnelBase(%q) = %v, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	if _, err := tunnelBase("", &net.UnixAddr{Name: "socket"}); err == nil {
		t.Error("accepted default tunnel base without a visitor port")
	}
}
