package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/tinypods-tunnel/internal/relay"
)

// The subprocess uses the real argument parser, signal handlers, and exit codes.
func TestMain(m *testing.M) {
	if os.Getenv("TINYPOD_TEST_CLI") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func cli(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), "TINYPOD_TEST_CLI=1", "TINYPOD_CA_FILE=")
	return cmd
}

func TestCommandErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantCode  int
		wantError string
	}{
		{"missing args", nil, 2, "usage: tinypod"},
		{"unknown scheme", []string{"udp", "3000"}, 2, "usage: tinypod"},
		{"not a number", []string{"http", "nope"}, 2, "port must be an integer"},
		{"out of range", []string{"http", "65536"}, 2, "port must be an integer"},
		{"relay error", []string{"https", "443"}, 1, "TINYPOD_RELAY must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := cli(t, tc.args...)
			cmd.Env = append(cmd.Env, "TINYPOD_RELAY=invalid")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tc.wantCode || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.wantError) {
				t.Fatalf("exit %v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
			}
		})
	}
}

func TestCommandPrintsAssignedTunnel(t *testing.T) {
	base := &url.URL{Scheme: "https", Host: "tunnel.test"}
	r := relay.New(base)
	control := httptest.NewServer(r.AgentHandler())
	defer control.Close()
	defer r.Close()

	cmd := cli(t, "http", "3000")
	cmd.Env = append(cmd.Env, "TINYPOD_RELAY=ws"+strings.TrimPrefix(control.URL, "http")+"/v1/connect")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(strings.TrimSpace(line))
	if err != nil || endpoint.Scheme != base.Scheme || !strings.HasSuffix(endpoint.Host, "."+base.Host) || strings.TrimSuffix(endpoint.Host, "."+base.Host) == "" {
		t.Fatalf("invalid advertised tunnel %q", line)
	}
	if runtime.GOOS == "windows" {
		r.Close()
		var exit *exec.ExitError
		if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(stderr.String(), "relay disconnected") {
			t.Fatalf("relay shutdown: exit %v, stderr %q", err, stderr.String())
		}
		return
	}
	if err = cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil || stderr.Len() != 0 {
		t.Fatalf("interrupt: exit %v, stderr %q", err, stderr.String())
	}
}
