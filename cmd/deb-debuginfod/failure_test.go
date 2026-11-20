package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/pwndbg/debuginfod.pwndbg.re/srcindex"
)

var droppedPkg = pkgRef{Source: "glibc", Version: "2.41-10", Component: "main"}

// unpackerAnswering stands in for cmd/deb-unpack and counts how often it is
// asked - which is what stands for a request to snapshot.debian.org or
// Launchpad behind it.
func unpackerAnswering(t *testing.T, status int, ttl time.Duration) (*srv, *int64) {
	t.Helper()
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		http.Error(w, "snapshot: 404 Not Found", status)
	}))
	t.Cleanup(up.Close)
	return &srv{
		unpacker: up.URL,
		hc:       up.Client(),
		indexes:  map[string]*srcindex.Index{},
		missing:  expirable.NewLRU[string, error](100, nil, ttl),
		failed:   expirable.NewLRU[string, error](100, nil, ttl),
	}, &calls
}

// A package in neither the pool nor the fallback is an answer. A debugger
// stepping through fifty files of it must make one attempt, not fifty.
func TestMissingPackageIsRememberedAndReportedAsMissing(t *testing.T) {
	s, calls := unpackerAnswering(t, http.StatusNotFound, time.Hour)
	for i := 0; i < 5; i++ {
		_, err := s.indexFor(context.Background(), droppedPkg)
		if !errors.Is(err, errSourcesMissing) {
			t.Fatalf("attempt %d: err = %v, want errSourcesMissing (so the client gets a 404)", i, err)
		}
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("unpacker asked %d times, want 1", n)
	}
}

// Any other failure is remembered too, but only briefly: long enough to
// collapse a burst, short enough that a blip does not become an outage.
func TestTransientFailureIsRememberedBriefly(t *testing.T) {
	s, calls := unpackerAnswering(t, http.StatusBadGateway, 50*time.Millisecond)
	for i := 0; i < 5; i++ {
		_, err := s.indexFor(context.Background(), droppedPkg)
		if err == nil || errors.Is(err, errSourcesMissing) {
			t.Fatalf("attempt %d: err = %v, want a failure that is not 'missing'", i, err)
		}
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("unpacker asked %d times within the TTL, want 1", n)
	}

	time.Sleep(100 * time.Millisecond)
	s.indexFor(context.Background(), droppedPkg)
	if n := atomic.LoadInt64(calls); n != 2 {
		t.Errorf("unpacker asked %d times after the TTL, want 2 - the failure must expire", n)
	}
}

// A client hanging up says nothing about the package. singleflight makes every
// waiter share the leader's fate, so caching it would hand one disconnect to
// everyone asking for that package.
func TestCancelledRequestIsNotRemembered(t *testing.T) {
	s, calls := unpackerAnswering(t, http.StatusBadGateway, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.indexFor(ctx, droppedPkg)

	s.indexFor(context.Background(), droppedPkg)
	if n := atomic.LoadInt64(calls); n != 1 {
		// The cancelled call never reaches the unpacker; the second must.
		t.Errorf("unpacker asked %d times, want 1 - the cancelled attempt was cached", n)
	}
}

// 502 when the failure happened behind us - the unpacker reporting that the pool
// or fallback failed, or the unpacker unreachable - and 500 when it was ours.
func TestPrepareStatusSaysWhoseFailureItWas(t *testing.T) {
	for status, want := range map[int]int{
		http.StatusBadGateway:          http.StatusBadGateway,
		http.StatusInternalServerError: http.StatusInternalServerError,
	} {
		s, _ := unpackerAnswering(t, status, time.Hour)
		_, err := s.indexFor(context.Background(), droppedPkg)
		if got := prepareStatus(err); got != want {
			t.Errorf("unpacker answered %d: status %d, want %d (err: %v)", status, got, want, err)
		}
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	s := &srv{unpacker: dead.URL, hc: http.DefaultClient, indexes: map[string]*srcindex.Index{}}
	_, err := s.indexFor(context.Background(), droppedPkg)
	if got := prepareStatus(err); got != http.StatusBadGateway {
		t.Errorf("unpacker unreachable: status %d, want 502 (err: %v)", got, err)
	}
}
