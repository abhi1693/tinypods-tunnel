package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/abhi1693/tinypods-tunnel/internal/relay"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address for tunnel visitors")
	agentListen := flag.String("agent-listen", "127.0.0.1:8081", "address for the outbound agent connection")
	publicURL := flag.String("public-url", "", "base HTTP(S) origin for generated tunnel subdomains (default http://localhost:<visitor-port>)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *listen, *agentListen, *publicURL); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, listen, agentListen, publicURL string) error {
	visitors, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer visitors.Close()
	agents, err := net.Listen("tcp", agentListen)
	if err != nil {
		return err
	}
	defer agents.Close()
	base, err := tunnelBase(publicURL, visitors.Addr())
	if err != nil {
		return err
	}
	return serve(ctx, visitors, agents, base)
}

func serve(ctx context.Context, visitors, agents net.Listener, base *url.URL) error {
	tunnel := relay.New(base)
	public := server(tunnel.PublicHandler())
	control := server(tunnel.AgentHandler())
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	context.AfterFunc(ctx, func() {
		tunnel.Close()
		public.Close()
		control.Close()
	})
	result := make(chan error, 2)
	go func() { result <- public.Serve(visitors) }()
	go func() { result <- control.Serve(agents) }()
	log.Printf("visitor listener %s, tunnel domain %s, agent listener %s", visitors.Addr(), base.Host, agents.Addr())
	var serveErr error
	for range 2 {
		err := <-result
		stop() // Stop the peer if either listener fails.
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, err)
		}
	}
	return serveErr
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func tunnelBase(raw string, listener net.Addr) (*url.URL, error) {
	if raw == "" {
		_, port, err := net.SplitHostPort(listener.String())
		if err != nil {
			return nil, err
		}
		raw = "http://" + net.JoinHostPort("localhost", port)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("-public-url must be an HTTP(S) origin")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return nil, fmt.Errorf("-public-url needs a DNS hostname for tunnel subdomains")
		}
		host = "localhost"
	}
	for _, label := range strings.Split(host, ".") {
		if !dnsLabel.MatchString(label) {
			return nil, fmt.Errorf("invalid base hostname in -public-url")
		}
	}
	// Leave room for a 26-character tunnel label and its separating dot.
	if len(host) > 226 {
		return nil, fmt.Errorf("base hostname too long for a tunnel subdomain")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return nil, fmt.Errorf("-public-url port must be between 1 and 65535")
		}
		host = net.JoinHostPort(host, port)
	}
	return &url.URL{Scheme: u.Scheme, Host: host}, nil
}

func server(handler http.Handler) *http.Server {
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
	}
}
