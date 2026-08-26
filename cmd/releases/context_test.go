package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedirectIsAccessLogged(t *testing.T) {
	db := &fakeLogger{}
	rec := requestTo(t, db, serveHost, "/releases/2026.02.18/pwndbg_2026.02.18_amd64.deb", map[string]string{
		"CF-Connecting-IP": "203.0.113.9",
		"CF-IPCountry":     "PL",
		"User-Agent":       "pwndbg-installer/1.0",
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}

	entries := db.all()
	if len(entries) != 1 {
		t.Fatalf("%d log entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Status != http.StatusFound {
		t.Errorf("status %d, want 302", e.Status)
	}
	// Version and file come from the route match, not from re-parsing the URI.
	if e.Version != "2026.02.18" || e.File != "pwndbg_2026.02.18_amd64.deb" {
		t.Errorf("version/file = %q / %q", e.Version, e.File)
	}
	if e.RequestURI != "/releases/2026.02.18/pwndbg_2026.02.18_amd64.deb" {
		t.Errorf("request_uri %q", e.RequestURI)
	}
	if e.RemoteIP.String() != "203.0.113.9" {
		t.Errorf("remote_ip %q, want the CF-Connecting-IP value", e.RemoteIP)
	}
	if e.Country != "PL" {
		t.Errorf("country %q, want PL", e.Country)
	}
	if e.UserAgent != "pwndbg-installer/1.0" {
		t.Errorf("user_agent %q", e.UserAgent)
	}
}

// A request that never became a download still has to be recorded, or a
// misrouted port looks like no traffic at all instead of a wrong Host.
func TestForeignHostIsLoggedAsRejected(t *testing.T) {
	db := &fakeLogger{}
	requestTo(t, db, "example.com", "/releases/v1/x.deb", nil)

	entries := db.all()
	if len(entries) != 1 {
		t.Fatalf("%d log entries, want 1", len(entries))
	}
	if entries[0].Status != http.StatusNotFound {
		t.Errorf("status %d, want 404", entries[0].Status)
	}
}

// The catch-all redirect answers every path a crawler invents. Logging it would
// put that traffic into every number on /stats, so it is deliberately outside the
// middleware - this pins that.
func TestCatchAllRedirectIsNotLogged(t *testing.T) {
	db := &fakeLogger{}
	for _, path := range []string{"/", "/wp-login.php", "/releases"} {
		requestTo(t, db, serveHost, path, nil)
	}
	if n := len(db.all()); n != 0 {
		t.Errorf("%d log entries for unregistered paths, want 0", n)
	}
}

func TestClientIPTrustsHeaderOnlyFromLoopback(t *testing.T) {
	for _, tc := range []struct {
		name, remote, header, want string
	}{
		{"loopback with header", "127.0.0.1:5555", "203.0.113.9", "203.0.113.9"},
		{"loopback no header", "127.0.0.1:5555", "", "127.0.0.1"},
		// Nothing routable should reach this service, but if it ever does the
		// header is forgeable and must not be believed.
		{"routable with header", "198.51.100.7:5555", "203.0.113.9", "198.51.100.7"},
		{"loopback garbage header", "127.0.0.1:5555", "not-an-ip", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/releases/v/f", nil)
			req.RemoteAddr = tc.remote
			if tc.header != "" {
				req.Header.Set("CF-Connecting-IP", tc.header)
			}
			if got := clientIP(req).String(); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// A failing insert must not change what the client gets: the redirect has already
// been written by the time the row is attempted.
func TestAccessLogFailureDoesNotBreakRedirect(t *testing.T) {
	db := &fakeLogger{err: http.ErrHandlerTimeout}
	rec := requestTo(t, db, serveHost, "/releases/2026.02.18/x.deb", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("location"), "https://github.com/") {
		t.Errorf("location %q", rec.Header().Get("location"))
	}
}
