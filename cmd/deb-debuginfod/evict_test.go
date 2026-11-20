package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pwndbg/debuginfod.pwndbg.re/srcindex"
)

// fakeUnpacker stands in for cmd/deb-unpack and records what was deleted.
type fakeUnpacker struct {
	mu      sync.Mutex
	trees   []tree
	deleted []string
	touched []string
	failOn  map[string]bool
}

func (f *fakeUnpacker) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /trees", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.trees)
	})
	mux.HandleFunc("POST /touch/{component}/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.touched = append(f.touched, r.PathValue("component")+"/"+r.PathValue("name"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /tree/{component}/{name}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("component") + "/" + r.PathValue("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failOn[key] {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		f.deleted = append(f.deleted, key)
		for i, tr := range f.trees {
			if tr.Key == key {
				f.trees = append(f.trees[:i], f.trees[i+1:]...)
				break
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (f *fakeUnpacker) gone() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func evictSrv(t *testing.T, f *fakeUnpacker) *srv {
	t.Helper()
	backend := f.server(t)
	return &srv{
		unpacker: backend.URL,
		hc:       backend.Client(),
		indexes:  map[string]*srcindex.Index{},
		lastUsed: map[string]time.Time{},
	}
}

func TestEvictLeavesEverythingWhenUnderBudget(t *testing.T) {
	f := &fakeUnpacker{trees: []tree{
		{Key: "main/glibc_1", Bytes: 100},
		{Key: "main/zlib_1", Bytes: 100},
	}}
	s := evictSrv(t, f)
	if err := s.evictOnce(context.Background(), 1000); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	if got := f.gone(); len(got) != 0 {
		t.Errorf("deleted %v while under budget", got)
	}
}

// The point of tracking use: a package fetched once and never touched again
// goes before one that has been serving a debugger all week, even though the
// busy one may have been unpacked first.
func TestEvictDropsLeastRecentlyUsedFirst(t *testing.T) {
	now := time.Now()
	f := &fakeUnpacker{trees: []tree{
		{Key: "main/busy_1", Bytes: 400, Modified: now.Add(-72 * time.Hour)},
		{Key: "main/idle_1", Bytes: 400, Modified: now},
		{Key: "main/other_1", Bytes: 400, Modified: now},
	}}
	s := evictSrv(t, f)
	s.lastUsed["main/busy_1"] = now
	s.lastUsed["main/idle_1"] = now.Add(-48 * time.Hour)
	s.lastUsed["main/other_1"] = now.Add(-1 * time.Hour)
	s.indexes["main/idle_1"] = &srcindex.Index{}

	// 1200 bytes present, budget 800 -> exactly one has to go.
	if err := s.evictOnce(context.Background(), 800); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	got := f.gone()
	if len(got) != 1 || got[0] != "main/idle_1" {
		t.Fatalf("deleted %v, want [main/idle_1]", got)
	}
	// The index has to go with it, or the next request serves paths whose files
	// no longer exist.
	s.mu.Lock()
	_, stillIndexed := s.indexes["main/idle_1"]
	s.mu.Unlock()
	if stillIndexed {
		t.Error("the index outlived the tree it points at")
	}
}

// A tree this process has never served has no use timestamp. Falling back to
// mtime at least orders those among themselves instead of leaving the order to
// map iteration.
func TestEvictFallsBackToModifiedTimeForUnseenTrees(t *testing.T) {
	now := time.Now()
	f := &fakeUnpacker{trees: []tree{
		{Key: "main/new_1", Bytes: 500, Modified: now},
		{Key: "main/old_1", Bytes: 500, Modified: now.Add(-96 * time.Hour)},
	}}
	s := evictSrv(t, f)
	if err := s.evictOnce(context.Background(), 600); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	if got := f.gone(); len(got) != 1 || got[0] != "main/old_1" {
		t.Errorf("deleted %v, want [main/old_1]", got)
	}
}

// One tree that will not delete must not stop the pass: the volume still needs
// to come down, and the next candidate may well succeed.
func TestEvictContinuesPastAFailedDelete(t *testing.T) {
	now := time.Now()
	f := &fakeUnpacker{
		trees: []tree{
			{Key: "main/stuck_1", Bytes: 500, Modified: now.Add(-96 * time.Hour)},
			{Key: "main/next_1", Bytes: 500, Modified: now.Add(-48 * time.Hour)},
			{Key: "main/fresh_1", Bytes: 500, Modified: now},
		},
		failOn: map[string]bool{"main/stuck_1": true},
	}
	s := evictSrv(t, f)
	if err := s.evictOnce(context.Background(), 600); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	got := f.gone()
	if len(got) == 0 || !strings.Contains(strings.Join(got, ","), "main/next_1") {
		t.Errorf("deleted %v, expected the pass to move on to main/next_1", got)
	}
}

// A zero or negative budget disables eviction rather than deleting everything -
// the alternative reading of "no space allowed" would empty the volume on the
// first tick after a typo in the config.
func TestEvictLoopRefusesAZeroBudget(t *testing.T) {
	f := &fakeUnpacker{trees: []tree{{Key: "main/glibc_1", Bytes: 1 << 30}}}
	s := evictSrv(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.EvictLoop(ctx, time.Millisecond, 0) // returns immediately
	if got := f.gone(); len(got) != 0 {
		t.Errorf("deleted %v with eviction disabled", got)
	}
}

// The write-through is what keeps eviction ordering meaningful across a restart:
// the map is empty on every start, and this service is redeployed far more often
// than a 20 GiB budget fills.
//
// It has to go through the unpacker because the serving container mounts the
// volume read-only - deliberately, so the single writer stays the single writer.
func TestTouchIsWrittenThroughToTheUnpacker(t *testing.T) {
	f := &fakeUnpacker{}
	s := evictSrv(t, f)

	s.touch(context.Background(), "main/glibc_1")

	f.mu.Lock()
	got := append([]string(nil), f.touched...)
	f.mu.Unlock()
	if len(got) != 1 || got[0] != "main/glibc_1" {
		t.Fatalf("unpacker saw %v, want one touch of main/glibc_1", got)
	}
	if _, ok := s.lastUsed["main/glibc_1"]; !ok {
		t.Error("the in-memory map was not updated")
	}
}

// A round trip on every request would put the unpacker on the hot path for no
// benefit: eviction orders by hours, not by milliseconds.
func TestTouchIsThrottled(t *testing.T) {
	f := &fakeUnpacker{}
	s := evictSrv(t, f)

	for range 5 {
		s.touch(context.Background(), "main/glibc_1")
	}

	f.mu.Lock()
	n := len(f.touched)
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("unpacker was called %d times for five requests, want 1", n)
	}
}

// A failing or absent unpacker must not break serving: this is bookkeeping, and
// losing it costs eviction ordering after the next restart, not correctness now.
func TestTouchSurvivesAnUnreachableUnpacker(t *testing.T) {
	f := &fakeUnpacker{}
	s := evictSrv(t, f)
	s.unpacker = "http://127.0.0.1:1" // nothing listens here

	s.touch(context.Background(), "main/glibc_1")

	if _, ok := s.lastUsed["main/glibc_1"]; !ok {
		t.Error("a failed write-through lost the in-memory record too")
	}
}
