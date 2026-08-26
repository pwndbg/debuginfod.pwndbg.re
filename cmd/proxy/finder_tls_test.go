package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Verification is skipped for loopback and nowhere else. Getting this wrong in
// the permissive direction means silently not authenticating a third-party
// upstream - the failure would be invisible, which is why it is pinned here
// rather than left to review.
func TestOnlyLoopbackSkipsTLSVerification(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8034", true},
		{"[::1]:8034", true},
		{"localhost:8034", true},
		{"127.5.6.7:443", true}, // the whole 127/8 is loopback
		{"debuginfod.fedoraproject.org:443", false},
		{"192.168.1.10:443", false},
		{"10.0.0.1:443", false},
		{"1.2.3.4:443", false},
		{"no-port-here", false},
		{"", false},
		// Nothing that merely mentions loopback may pass.
		{"127.0.0.1.evil.com:443", false},
		{"localhost.evil.com:443", false},
	} {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// End to end: a self-signed certificate on loopback is accepted, which is what
// lets this proxy talk to cmd/nix-debuginfod at all.
func TestFetchAcceptsLoopbackSelfSignedCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DEBUGINFO"))
	}))
	defer srv.Close()

	f := NewDebugInfoFinder(nil)
	resp, err := f.Fetch(context.Background(), srv.URL+"/buildid/x/debuginfo")
	if err != nil {
		t.Fatalf("fetching a loopback self-signed server: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "DEBUGINFO" {
		t.Errorf("body = %q", body)
	}
}

// The same certificate must be rejected when it is not loopback. Only the
// ADDRESS differs - the dial is redirected back at the same test server - so
// this isolates the decision rather than testing DNS.
func TestFetchRejectsSelfSignedCertOffLoopback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DEBUGINFO"))
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "https://")

	f := NewDebugInfoFinder(nil)
	tr := f.client.Transport.(*http.Transport)
	outer := tr.DialTLSContext
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Keep the non-loopback name for the TLS decision, dial the real server.
		if strings.HasPrefix(addr, "self-signed.example.com:") {
			cfg := &tls.Config{NextProtos: []string{"h2", "http/1.1"}, ServerName: "self-signed.example.com"}
			return (&tls.Dialer{Config: cfg}).DialContext(ctx, network, target)
		}
		return outer(ctx, network, addr)
	}

	_, err := f.Fetch(context.Background(), "https://self-signed.example.com/buildid/x/debuginfo")
	if err == nil {
		t.Fatal("a self-signed certificate was accepted off loopback")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}
