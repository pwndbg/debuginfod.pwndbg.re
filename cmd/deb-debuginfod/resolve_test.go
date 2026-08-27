package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

func testSrv(t *testing.T, h http.HandlerFunc) (*srv, *int64) {
	t.Helper()
	var calls int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		h(w, r)
	}))
	t.Cleanup(backend.Close)
	return &srv{
		debian:   backend.URL,
		hc:       backend.Client(),
		resolved: expirable.NewLRU[string, pkgRef](resolveCacheSize, nil, resolveCacheTTL),
	}, &calls
}

const glibcArchive = "/srv/mirror/debian-debug/dbg-main/g/glibc/libc6-dbg_2.41-12+deb13u3_amd64.deb"

// A debugger stepping through one function asks for dozens of files from the
// same build ID. Each one used to cost a round trip to Debian - measured at
// ~140 ms against ~2 ms once the answer is remembered.
func TestResolveIsCached(t *testing.T) {
	s, calls := testSrv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-debuginfod-archive", glibcArchive)
	})
	for range 5 {
		pkg, err := s.resolve(context.Background(), "c495b62edadd6c356265942ec1282d98058a7b41")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if pkg.Source != "glibc" || pkg.Version != "2.41-12+deb13u3" {
			t.Fatalf("got %+v", pkg)
		}
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("asked Debian %d times, want 1", n)
	}
}

// A failure must NOT be remembered. It is usually "Debian was slow", not "this
// build ID does not exist", and a 24-hour TTL would turn a blip into a day-long
// outage for that build ID.
func TestResolveDoesNotCacheFailures(t *testing.T) {
	s, calls := testSrv(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	for range 3 {
		if _, err := s.resolve(context.Background(), "deadbeef"); err == nil {
			t.Fatal("expected an error")
		}
	}
	if n := atomic.LoadInt64(calls); n != 3 {
		t.Errorf("asked Debian %d times, want 3 - a failure was cached", n)
	}
}

// A 200 with no archive header is not a usable answer, and must not be cached
// as one either.
func TestResolveRejectsMissingHeader(t *testing.T) {
	s, _ := testSrv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if _, err := s.resolve(context.Background(), "abc"); err == nil {
		t.Fatal("expected an error when the archive header is absent")
	}
}

// A debugger opens several files at once. Without coalescing, a cold build ID
// makes one identical request to Debian per file.
func TestResolveCoalescesConcurrentLookups(t *testing.T) {
	s, calls := testSrv(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("x-debuginfod-archive", glibcArchive)
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.resolve(context.Background(), "same-build-id"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("asked Debian %d times for one cold build ID, want 1", n)
	}
}
