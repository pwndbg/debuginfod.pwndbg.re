package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// captureHook collects log lines and lets a probe run at exactly the moment a
// given line is written. logrus calls Fire() synchronously from the log.*() call
// site, so the probe observes precisely the lock state a real write(2) to stderr
// would.
type captureHook struct {
	mu    sync.Mutex
	lines []string
	match string        // the probe fires only for a line containing this text
	probe func()        // run at the moment of logging
	fired chan struct{} // closed after the first match
	once  sync.Once
}

func (h *captureHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h *captureHook) Fire(e *logrus.Entry) error {
	h.mu.Lock()
	h.lines = append(h.lines, e.Message)
	h.mu.Unlock()
	if h.match != "" && strings.Contains(e.Message, h.match) {
		if h.probe != nil {
			h.probe()
		}
		h.once.Do(func() { close(h.fired) })
	}
	return nil
}

func (h *captureHook) has(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// installHook attaches a hook to the standard logger and detaches it after the test.
//
// ReplaceHooks, and NOT a bare write to std.Hooks: logrus reads that field under its
// own mutex in (*Entry).fireHooks, so swapping it without the lock is a data race -
// and a reachable one in these tests, because the goroutine cleaning up after a
// download outlives Serve itself and may log exactly while t.Cleanup restores the
// hooks.
func installHook(t *testing.T, h *captureHook) {
	t.Helper()
	std := logrus.StandardLogger()
	hooks := make(logrus.LevelHooks)
	hooks.Add(h)
	prev := std.ReplaceHooks(hooks)
	t.Cleanup(func() { std.ReplaceHooks(prev) })
}

// Regression: the teardown in download() logged a persistence failure WHILE HOLDING
// e.mu. logrus writes to stderr synchronously under its own mutex, and in a
// container stderr is a pipe to the log driver - a full pipe blocks write(2).
// Under e.mu that would stall every follower of this entry, and because Serve holds
// the global c.mu while waiting for e.mu, requests for every other build ID as well.
// This test pins that e.mu is free at the moment of logging.
func TestDownloadDoesNotLogUnderEntryLock(t *testing.T) {
	const chunks, chunkSize = 2, 8 << 10
	up := newStepServer(t, chunks, chunkSize)
	up.releaseAll(chunks - 1)
	c := newTestCache(t, 0)

	const key = "k"
	// Force the "body delivered but persistence failed" path: put a DIRECTORY where
	// the .meta file should go, so the rename in writeFileAtomic must fail.
	// Non-empty on purpose - see the comment in TestFailedMetaPublishRemovesBlob.
	blob := c.blobPath(key)
	if err := os.MkdirAll(filepath.Join(blob+".meta", "zajete"), 0o755); err != nil {
		t.Fatal(err)
	}

	var entryLockFree bool
	h := &captureHook{
		match: "nie udalo sie utrwalic wpisu",
		fired: make(chan struct{}),
	}
	h.probe = func() {
		// The entry is still alive in inflight - removal happens below the log call.
		c.mu.Lock()
		e := c.inflight[key]
		c.mu.Unlock()
		if e == nil {
			return
		}
		// Acquire e.mu BLOCKING, in a separate goroutine. A single TryLock is no
		// good as a probe: cond.Wait re-acquires e.mu when it wakes, so a follower
		// may legitimately hold it for a moment. A blocking acquire waits such a
		// hold out, while still catching the thing that matters: if e.mu is held by
		// a goroutine that is CURRENTLY logging, it will not release it until the
		// line is written - and that write is waiting on this probe, so it never
		// will.
		got := make(chan struct{})
		go func() {
			e.mu.Lock()
			e.mu.Unlock() //nolint:staticcheck // the only question is whether it can be acquired
			close(got)
		}()
		select {
		case <-got:
			entryLockFree = true
		case <-time.After(2 * time.Second):
		}
	}
	installHook(t, h)

	w := newGateWriter(nil)
	// The body reaches the client in full - the failure concerns only the cache write.
	if err := c.Serve(context.Background(), key, w, "up", fetchFrom(up.URL)); err != nil {
		t.Fatalf("Serve zwrocil blad, choc cialo bylo kompletne: %v", err)
	}

	select {
	case <-h.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("nie zalogowano bledu utrwalenia - test nie sprawdzil tego, co mial")
	}
	if !entryLockFree {
		t.Error("e.mu bylo TRZYMANE podczas logowania: zapchany stderr zatrzyma cala usluge")
	}
}

// blockReads removes read permission and reports whether that ACTUALLY took effect.
// It checks by trying rather than via os.Geteuid(): as root, chmod blocks nothing,
// but the same holds for a process with CAP_DAC_OVERRIDE and for filesystems that
// ignore permission bits. A euid gate would then PASS without running the test at
// all - and Dockerfile.proxy declares no USER, so production runs as root.
func blockReads(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		return false // readable despite chmod 0 - this cannot be tested here
	}
	return true
}

// Regression: the tree-walking function returned nil for EVERY error, so
// filepath.Walk structurally could not fail. An unreadable root yielded total=0,
// evictOnce concluded the cache was empty and deleted nothing - while the volume
// filled up. The error handling and its log line were dead code.
func TestEvictOnceFailsOnUnreadableRoot(t *testing.T) {
	c := newTestCache(t, 1000)
	if !blockReads(t, c.dir) {
		t.Skip("odczyt dziala mimo chmod 0 (root / CAP_DAC_OVERRIDE / fs bez uprawnien)")
	}

	if err := c.evictOnce(); err == nil {
		t.Fatal("evictOnce zwrocil nil dla nieczytalnego katalogu cache - eviction jest slepy")
	}
}

// A single unreadable shard must not abort the scan, but it MUST be visible:
// without that the total is understated and the early "we fit in the budget" return
// is falsely reassuring.
func TestEvictOnceWarnsAboutSkippedEntries(t *testing.T) {
	c := newTestCache(t, 1000)

	bad := filepath.Join(c.dir, "ab")
	if err := os.MkdirAll(filepath.Join(bad, "cd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !blockReads(t, bad) {
		t.Skip("odczyt dziala mimo chmod 0 (root / CAP_DAC_OVERRIDE / fs bez uprawnien)")
	}

	h := &captureHook{fired: make(chan struct{})}
	installHook(t, h)

	if err := c.evictOnce(); err != nil {
		t.Fatalf("pojedynczy nieczytelny shard nie powinien przerywac skanu: %v", err)
	}
	if !h.has("pominal nieczytelne wpisy") {
		t.Error("brak ostrzezenia o pominietych wpisach - zanizony total wyglada jak pusty cache")
	}
}

// openCached returned a bare false for six different situations: a missing .meta
// (an ordinary miss) and five faults that nothing logged - making them
// indistinguishable from a cold cache. They now leave a trace in the log.
//
// But they DELIBERATELY delete nothing, and that is what this test is about.
// Deleting the pair was in here briefly and was reverted: it bought nothing
// (os.Rename in download() overwrites the entry unconditionally, so it heals
// itself) and could cost everything - an error on this path need say nothing about
// the file. EMFILE is a process-wide condition, so on descriptor exhaustion it
// would delete exactly the blob the request is asking for, at the rate of traffic.
// For an archive whose upstream no longer has those symbols, that loss is
// permanent.
func TestOpenCachedKeepsFilesOnCorruptEntry(t *testing.T) {
	c := newTestCache(t, 0)
	blob := c.blobPath("k")
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("tresc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob+".meta", []byte("{to nie jest JSON"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &captureHook{fired: make(chan struct{})}
	installHook(t, h)

	if _, f, ok := c.openCached("k"); ok {
		f.Close()
		t.Fatal("openCached uznal uszkodzony wpis za wazny")
	}
	if !h.has("wpis nie do uzycia") {
		t.Error("usterka nie zostawila sladu w logu - nierozroznialna od zimnego cache")
	}
	for _, path := range []string{blob, blob + ".meta"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s zostal SKASOWANY: przy archiwum przelotny blad odczytu nie moze niszczyc danych (%v)",
				filepath.Base(path), err)
		}
	}
}

// A missing .meta is an ordinary miss, not a fault: it must neither make noise in
// the log nor delete the blob. This is also the window between renaming the blob and
// writing the .meta in download(), where the blob ALREADY exists and is valid.
func TestOpenCachedTreatsMissingMetaAsPlainMiss(t *testing.T) {
	c := newTestCache(t, 0)
	blob := c.blobPath("k")
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("tresc"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &captureHook{fired: make(chan struct{})}
	installHook(t, h)

	if _, f, ok := c.openCached("k"); ok {
		f.Close()
		t.Fatal("openCached zwrocil wpis bez .meta")
	}
	if h.has("wpis nie do uzycia") {
		t.Error("zwykle chybienie zalogowane jako usterka")
	}
	if _, err := os.Stat(blob); err != nil {
		t.Errorf("blob skasowany przy zwyklym chybieniu: %v", err)
	}
}

// Abandoned temp files were INVISIBLE to eviction in both roles at once: they did
// not count towards the budget and could not be evicted. The defer in download()
// cleaned them up, but `docker rm -f` in run.sh is a SIGKILL, so every deploy with
// downloads in flight left them on disk - and the volume is now persistent.
// The effect: CACHE_MAX_BYTES stopped describing reality, cumulatively.
func TestEvictOnceSweepsAbandonedTempFiles(t *testing.T) {
	c := newTestCache(t, 1<<30) // huge budget: the sweep must work REGARDLESS of the threshold
	shard := filepath.Dir(c.blobPath("k"))
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(name string, age time.Duration) string {
		p := filepath.Join(shard, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 1024), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}

	hex64 := strings.Repeat("ab", 32)
	stale := write(hex64+".tmp-111111", cacheFetchTimeout+time.Minute)
	fresh := write(hex64+".tmp-222222", time.Minute)
	blob := write(hex64, time.Hour)

	if err := c.evictOnce(); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("porzucony .tmp starszy niz cacheFetchTimeout przetrwal - budzet dalej go nie widzi")
	}
	// A younger one may belong to a download running RIGHT NOW, ours or one from an
	// old container that has not exited yet. Deleting it would cut off that transfer.
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("swiezy .tmp zostal skasowany: %v", err)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Errorf("blob zostal skasowany mimo ogromnego budzetu: %v", err)
	}
}
