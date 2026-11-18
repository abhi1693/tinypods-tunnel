package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// Let subprocess tests observe the fixture's CLI diagnostics and exit code.
func TestMain(m *testing.M) {
	if os.Getenv("TINYPOD_TEST_ORIGIN_COMMAND") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestOriginCommandReportsStartupErrors(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, port, err := net.SplitHostPort(busy.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	missingCert := filepath.Join(t.TempDir(), "missing.crt")
	for _, tc := range []struct {
		name, diagnostic string
		args             []string
	}{
		{"missing private key", "supply both -cert and -key", []string{"-cert", missingCert}},
		{"missing certificate", "supply both -cert and -key", []string{"-key", "key.pem"}},
		{"failed HTTP listen", busy.Addr().String(), []string{"-port", port}},
		{"invalid TLS certificate", missingCert, []string{"-port", "0", "-cert", missingCert, "-key", "key.pem"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], tc.args...)
			cmd.Env = append(os.Environ(), "TINYPOD_TEST_ORIGIN_COMMAND=1")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), tc.diagnostic) {
				t.Fatalf("origin exited with %v, output %q; want status 1 and %q", err, out, tc.diagnostic)
			}
		})
	}
}

func TestOriginRoutes(t *testing.T) {
	origin := httptest.NewTLSServer(handler())
	defer origin.Close()
	client := origin.Client()
	payload := []byte{0x00, 0x01, 0x80, 0xff}
	for _, tc := range []struct {
		method, path, contentType string
		status                    int
		body, want                []byte
	}{
		{"GET", "/health?id=delivery", "text/plain", http.StatusOK, nil, []byte("delivery")},
		{"POST", "/webhook?id=delivery", "application/octet-stream", http.StatusAccepted, payload, payload},
		{"GET", "/webhook", "text/plain; charset=utf-8", http.StatusMethodNotAllowed, nil, []byte("Method Not Allowed\n")},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, origin.URL+tc.path, bytes.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Delivery-ID", "delivery")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != tc.status || resp.Header.Get("Content-Type") != tc.contentType || !bytes.Equal(body, tc.want) {
			t.Errorf("%s %s returned HTTP %d, headers %v, body %q, err %v", tc.method, tc.path, resp.StatusCode, resp.Header, body, err)
		}
		if tc.method == "POST" && resp.Header.Get("X-Delivery-ID") != "delivery" {
			t.Error("webhook response lost its delivery ID")
		}
	}
}

func TestWebhookRejectsInvalidDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, queryID, headerID string
		body                    io.Reader
		status                  int
		message                 string
	}{
		{"missing ID", "", "", nil, http.StatusBadRequest, "delivery ID mismatch"},
		{"mismatched IDs", "a", "b", nil, http.StatusBadRequest, "delivery ID mismatch"},
		{"read failure", "a", "a", iotest.ErrReader(io.ErrUnexpectedEOF), http.StatusBadRequest, "could not read body"},
		{"oversized body", "a", "a", bytes.NewReader(make([]byte, 1<<20+1)), http.StatusRequestEntityTooLarge, "body too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/webhook?id="+tc.queryID, tc.body)
			req.Header.Set("X-Delivery-ID", tc.headerID)
			w := httptest.NewRecorder()
			handler().ServeHTTP(w, req)
			if w.Code != tc.status || strings.TrimSpace(w.Body.String()) != tc.message {
				t.Errorf("got HTTP %d, %q; want HTTP %d, %q", w.Code, w.Body.String(), tc.status, tc.message)
			}
		})
	}
}
