package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Unregistered paths - "/" first among them - send people to the project page
// instead of httprouter's bare 404.
func TestUnknownPathRedirectsToProject(t *testing.T) {
	var s serverSrv
	s.InitRouter()

	for _, path := range []string{"/", "/favicon.ico", "/robots.txt", "/a/b/c"} {
		rec := httptest.NewRecorder()
		s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusFound {
			t.Errorf("%s: status %d, want %d", path, rec.Code, http.StatusFound)
		}
		if got := rec.Header().Get("location"); got != projectURL {
			t.Errorf("%s: location %q, want %q", path, got, projectURL)
		}
	}
}

// The catch-all must not swallow the routes that already exist.
func TestRegisteredRoutesStillWork(t *testing.T) {
	var s serverSrv
	s.InitRouter()

	rec := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/status: status %d, want 200", rec.Code)
	}
}
