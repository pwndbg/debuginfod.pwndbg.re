package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// singleflight collapses duplicate work on the *same* store path; it says
// nothing about how many different ones run at once. Each build is a NAR
// download plus an mkfs.erofs process, so that number has to be bounded - this
// is the one job the deleted worker pool did that singleflight does not.
//
// Deliberately exercises buildOnce rather than Ensure: the semaphore lives
// there, and testing it this way needs no mount, so it runs everywhere instead
// of only on a privileged Linux box. That is not incidental - the dev kernel
// caps max_loop at 4, so an Ensure-based version of this test failed on loop
// device exhaustion rather than on the thing it was checking.
func TestBuildOnceBoundsConcurrency(t *testing.T) {
	const limit = 2

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	build := func(tmp string) error {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond) // long enough that unbounded callers overlap
		mu.Lock()
		inFlight--
		mu.Unlock()
		return os.WriteFile(tmp, []byte("image"), 0o644)
	}

	dir := t.TempDir()
	s := newStore(nil, dir, filepath.Join(dir, "store"), filepath.Join(dir, "entry"), limit)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.buildOnce(context.Background(),
				filepath.Join(dir, "img", "distinct-"+string(rune('a'+i))+".erofs"), build)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("build #%d: %v", i, err)
		}
	}
	mu.Lock()
	got := peak
	mu.Unlock()
	if got > limit {
		t.Errorf("peak concurrent builds = %d, want at most %d", got, limit)
	}
	if got < 2 {
		t.Errorf("peak was %d - the test never overlapped, so it proves nothing", got)
	}
}

// An image already on disk must not wait for a build slot, or a cache hit would
// queue behind unrelated cold fetches.
func TestBuildOnceSkipsWorkForAnExistingImage(t *testing.T) {
	dir := t.TempDir()
	s := newStore(nil, dir, filepath.Join(dir, "store"), filepath.Join(dir, "entry"), 1)

	image := filepath.Join(dir, "img", "x.erofs")
	if err := os.MkdirAll(filepath.Dir(image), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}

	called := false
	if err := s.buildOnce(context.Background(), image, func(string) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("rebuilt an image that was already on disk")
	}
}

// A caller that gives up while queued must not hold the slot.
func TestBuildOnceRespectsCancellation(t *testing.T) {
	dir := t.TempDir()
	s := newStore(nil, dir, filepath.Join(dir, "store"), filepath.Join(dir, "entry"), 1)

	release, err := s.acquire(context.Background()) // occupy the only slot
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err = s.buildOnce(ctx, filepath.Join(dir, "img", "y.erofs"), func(string) error {
		t.Error("build ran despite a cancelled context")
		return nil
	})
	if err == nil {
		t.Fatal("want a context error")
	}
}
