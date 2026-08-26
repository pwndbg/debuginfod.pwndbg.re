package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The .meta status goes straight into w.WriteHeader, and net/http panics on a
// status outside the valid range. A file left by an older version, by a crash, or
// found in a shared directory may have no status at all.
func TestOpenCachedRejectsInvalidStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta string
		want bool
	}{
		{"brak pola status", `{"size":5,"headers":{}}`, false},
		{"status 0", `{"status":0,"size":5,"headers":{}}`, false},
		{"status 99", `{"status":99,"size":5,"headers":{}}`, false},
		{"status 1000", `{"status":1000,"size":5,"headers":{}}`, false},
		{"status 200", `{"status":200,"size":5,"headers":{}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache(t, 0)
			blob := c.blobPath("k")
			if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(blob, []byte("12345"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(blob+".meta", []byte(tc.meta), 0o644); err != nil {
				t.Fatal(err)
			}
			_, f, ok := c.openCached("k")
			if f != nil {
				f.Close()
			}
			if ok != tc.want {
				t.Errorf("openCached ok=%v, oczekiwano %v", ok, tc.want)
			}
		})
	}
}

// The retry branch (avail > 0, read returns nothing) must observe the end of the
// download. Without that a follower spun ~1000 times a second until the client
// disconnected.
func TestEntryReaderRetryBranchSeesDownloadEnd(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "blob")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	tmp.Write([]byte("abc")) // shorter than the announced e.size

	for _, tc := range []struct {
		name    string
		setErr  error
		wantErr error
	}{
		{"lider zglosil blad", io.ErrClosedPipe, io.ErrClosedPipe},
		{"lider skonczyl, plik krotszy", nil, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCacheEntry()
			e.size = 100 // more than what is on disk
			e.done = true
			e.err = tc.setErr
			r := &entryReader{ctx: context.Background(), e: e, f: tmp, off: 3}

			done := make(chan error, 1)
			go func() { _, err := r.Read(make([]byte, 32)); done <- err }()

			select {
			case got := <-done:
				if !errors.Is(got, tc.wantErr) {
					t.Errorf("Read zwrocil %v, oczekiwano %v", got, tc.wantErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Read nie wrocil - petla ponawiania nie widzi konca pobierania")
			}
		})
	}
}

// With no free slot the cache must be bypassed IMMEDIATELY rather than making the
// client wait and then failing with 500. No entry may be created in the process.
func TestServeReturnsBusyWithoutBlockingOrPublishing(t *testing.T) {
	c := newTestCache(t, 0)
	for i := 0; i < maxConcurrentDownloads; i++ {
		c.sem <- struct{}{}
	}

	w := newGateWriter(nil)
	start := time.Now()
	err := c.Serve(context.Background(), "k", w, "up",
		func(context.Context) (*http.Response, error) {
			t.Error("upstream odpytany mimo braku slotu")
			return nil, nil
		})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCacheBusy) {
		t.Fatalf("err=%v, oczekiwano ErrCacheBusy", err)
	}
	if elapsed > time.Second {
		t.Errorf("czekano %v - powinno wrocic natychmiast", elapsed)
	}
	if len(w.body()) != 0 || w.hdr.Get("x-cache") != "" {
		t.Error("cos poszlo do klienta mimo ErrCacheBusy")
	}
	c.mu.Lock()
	n := len(c.inflight)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("powstalo %d wpisow w inflight, oczekiwano 0", n)
	}
}

// If publishing the .meta fails, the blob must NOT survive: it would be
// unservable (openCached rejects it) and invisible to size accounting.
func TestFailedMetaPublishRemovesBlob(t *testing.T) {
	const chunks, chunkSize = 2, 1 << 10
	up := newStepServer(t, chunks, chunkSize)
	up.releaseAll(chunks - 1)
	c := newTestCache(t, 0)

	// A directory where the .meta should go makes renaming onto that path always
	// fail. Whether it is non-empty makes no difference - renaming a file onto a
	// directory returns EISDIR either way. Left as is, because it does no harm.
	blob := c.blobPath("k")
	if err := os.MkdirAll(filepath.Join(blob+".meta", "zajete"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", w, "up", fetchFrom(up.URL)); err != nil {
		t.Fatalf("klient powinien dostac komplet mimo awarii zapisu: %v", err)
	}
	if !bytes.Equal(bodyOf(t, w), up.payload) {
		t.Error("klient nie dostal kompletnej tresci")
	}

	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Errorf("blob zostal na dysku mimo nieudanej publikacji .meta (err=%v)", err)
	}
}
