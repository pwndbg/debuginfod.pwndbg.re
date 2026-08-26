package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeLogger stands in for *dbSrv so the router tests need no ClickHouse.
type fakeLogger struct {
	mu      sync.Mutex
	entries []AccessLogEntry
	err     error
}

func (f *fakeLogger) AccessLog(_ context.Context, e AccessLogEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return f.err
}

func (f *fakeLogger) all() []AccessLogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AccessLogEntry(nil), f.entries...)
}

func requestTo(t *testing.T, db accessLogger, host, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	// Production reaches this service through cloudflared on the same host, so
	// RemoteAddr is always loopback - which is what makes CF-Connecting-IP
	// trusted. httptest defaults to a routable address, where it is not.
	req.RemoteAddr = "127.0.0.1:12345"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	newRouter(db, nil).ServeHTTP(rec, req)
	return rec
}

func request(t *testing.T, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	return requestTo(t, &fakeLogger{}, host, path, nil)
}

func TestReleaseRedirect(t *testing.T) {
	rec := request(t, serveHost, "/releases/2024.02.14/pwndbg.tar.gz")

	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusFound)
	}
	want := "https://github.com/pwndbg/pwndbg/releases/download/2024.02.14/pwndbg.tar.gz"
	if got := rec.Header().Get("location"); got != want {
		t.Errorf("location %q, want %q", got, want)
	}
}

// cloudflared routes by hostname, but nothing else pointed at this port should
// be able to use it as an open redirector to the pwndbg release namespace.
func TestReleaseRejectsOtherHosts(t *testing.T) {
	for _, host := range []string{"debuginfod.pwndbg.re", "example.com", "127.0.0.1:8033"} {
		rec := request(t, host, "/releases/v1/x.tar.gz")
		if rec.Code != http.StatusNotFound {
			t.Errorf("host %s: status %d, want 404", host, rec.Code)
		}
	}
}

// A port on the Host header must not defeat the check.
func TestHostMatchesIgnoresPortAndCase(t *testing.T) {
	for _, host := range []string{serveHost, serveHost + ":443", "RELEASES.PWNDBG.RE"} {
		if !hostMatches(host) {
			t.Errorf("hostMatches(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"", "pwndbg.re", "evil-releases.pwndbg.re"} {
		if hostMatches(host) {
			t.Errorf("hostMatches(%q) = true, want false", host)
		}
	}
}

func TestUnknownPathRedirectsToProject(t *testing.T) {
	for _, path := range []string{"/", "/releases", "/anything"} {
		rec := request(t, serveHost, path)
		if rec.Code != http.StatusFound {
			t.Errorf("%s: status %d, want 302", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("location"); got != projectURL {
			t.Errorf("%s: location %q, want %q", path, got, projectURL)
		}
	}
}
