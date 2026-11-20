package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The production case: debuginfod.debian.net has dropped 9bee4eb8...c1a1f7cf
// (glibc 2.43-4), we still serve its debuginfo from cache, and the sources are
// still in Debian's pool. The delegate can only find them if we tell it the
// archive, because asking Debian again is exactly the lookup that now fails.
const expiredBuildID = "9bee4eb8329d9d71d9f59a08465751cbc1a1f7cf"
const expiredArchive = "/srv/mirror/debian-debug/dbg-main/g/glibc/libc6-dbg_2.43-4_amd64.deb"

// headerRecorder answers 200 and remembers the archive header of each request.
type headerRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newHeaderRecorder(t *testing.T) *headerRecorder {
	h := &headerRecorder{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Header.Get(delegateArchiveHeader))
		h.mu.Unlock()
		w.Write([]byte("ok"))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *headerRecorder) last(t *testing.T) string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) == 0 {
		t.Fatal("no request arrived")
	}
	return h.seen[len(h.seen)-1]
}

func finderWithState(t *testing.T, state BuildIDState) (*DebugInfoFinder, *headerRecorder) {
	t.Helper()
	rec := newHeaderRecorder(t)
	store := newFakeStore()
	store.states[state.BuildID] = state
	f := NewDebugInfoFinder(store)
	return f, rec
}

func fetchVia(t *testing.T, f *DebugInfoFinder, host *Server, url string) {
	t.Helper()
	resp, err := f.FetchFor(context.Background(), host, expiredBuildID, url+"/buildid/"+expiredBuildID+"/source/x.c")
	if err != nil {
		t.Fatalf("FetchFor: %v", err)
	}
	resp.Body.Close()
}

func TestDelegateIsToldTheRecordedArchive(t *testing.T) {
	f, rec := finderWithState(t, BuildIDState{
		BuildID: expiredBuildID, LastHost: "debian", LastSuccess: true,
		ResponseHeaders: StateHeaders{Archive: expiredArchive},
	})
	fetchVia(t, f, f.delegates["debian-src"], rec.URL)
	if got := rec.last(t); got != expiredArchive {
		t.Errorf("delegate got archive %q, want %q", got, expiredArchive)
	}
}

// A third-party upstream never gets it: it is our record, of no use to them,
// and the proxy otherwise sends upstream nothing beyond its fixed headers.
func TestUpstreamIsNotToldTheArchive(t *testing.T) {
	f, rec := finderWithState(t, BuildIDState{
		BuildID: expiredBuildID, LastHost: "debian", LastSuccess: true,
		ResponseHeaders: StateHeaders{Archive: expiredArchive},
	})
	fetchVia(t, f, f.servers["debian"], rec.URL)
	if got := rec.last(t); got != "" {
		t.Errorf("an upstream was sent archive %q", got)
	}
}

// Each delegate serves one distribution. A Debian archive handed to the Ubuntu
// delegate names a package that is not there; better to send nothing and let
// it ask its own upstream.
func TestDelegateIsNotToldAnotherDistributionsArchive(t *testing.T) {
	f, rec := finderWithState(t, BuildIDState{
		BuildID: expiredBuildID, LastHost: "debian", LastSuccess: true,
		ResponseHeaders: StateHeaders{Archive: expiredArchive},
	})
	fetchVia(t, f, f.delegates["ubuntu-src"], rec.URL)
	if got := rec.last(t); got != "" {
		t.Errorf("ubuntu-src was sent a Debian archive %q", got)
	}
}

// Rows written before response_headers existed have no archive. The request
// still goes through, without the header, and the delegate behaves as before.
func TestDelegateWithoutRecordedArchiveGetsNoHeader(t *testing.T) {
	f, rec := finderWithState(t, BuildIDState{
		BuildID: expiredBuildID, LastHost: "debian", LastSuccess: true,
	})
	fetchVia(t, f, f.delegates["debian-src"], rec.URL)
	if got := rec.last(t); got != "" {
		t.Errorf("got archive %q from a state that has none", got)
	}
}
