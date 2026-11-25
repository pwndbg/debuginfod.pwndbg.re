package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testDsc = `Format: 3.0 (quilt)
Source: coreutils
Files:
 aaaa 5 coreutils_8.32.orig.tar.xz
 bbbb 5 coreutils_8.32-4.1ubuntu1.2.debian.tar.xz
`

var sru = unpackRequest{Source: "coreutils", Version: "8.32-4.1ubuntu1.2", Component: "main"}

// archiveServer serves files by exact path and records every request, so a test
// can say where each byte came from, not just whether a fetch worked.
type archiveServer struct {
	*httptest.Server
	mu     sync.Mutex
	files  map[string]string
	status int // when non-zero, every request gets this instead
	asked  []string
}

func newArchiveServer(t *testing.T, files map[string]string) *archiveServer {
	a := &archiveServer{files: files}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.asked = append(a.asked, r.URL.RequestURI())
		status := a.status
		a.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		body, ok := a.files[r.URL.RequestURI()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *archiveServer) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asked...)
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const lpBase = "/+sourcefiles/coreutils/8.32-4.1ubuntu1.2/"

// archive.ubuntu.com drops superseded stable updates; Launchpad keeps them.
// The pool still has the orig tarball - it is shared with the current version -
// which is the trap: mixing sources would "work" here, so the test checks that
// everything came from Launchpad anyway.
func TestLaunchpadFallbackWhenThePoolDroppedTheVersion(t *testing.T) {
	pool := newArchiveServer(t, map[string]string{
		"/pool/main/c/coreutils/coreutils_8.32.orig.tar.xz": "pool!",
	})
	lp := newArchiveServer(t, map[string]string{
		lpBase + "coreutils_8.32-4.1ubuntu1.2.dsc":           testDsc,
		lpBase + "coreutils_8.32.orig.tar.xz":                "orig1",
		lpBase + "coreutils_8.32-4.1ubuntu1.2.debian.tar.xz": "debia",
	})
	s := &server{mirror: pool.URL, fallback: launchpad{base: lp.URL + "/+sourcefiles"}, hc: http.DefaultClient}

	work := t.TempDir()
	dsc, err := s.fetchSources(context.Background(), sru, work)
	if err != nil {
		t.Fatalf("fetchSources: %v", err)
	}
	if dsc != "coreutils_8.32-4.1ubuntu1.2.dsc" {
		t.Errorf("dsc = %q", dsc)
	}
	if got := read(t, filepath.Join(work, "coreutils_8.32.orig.tar.xz")); got != "orig1" {
		t.Errorf("orig tarball = %q, want Launchpad's - a package must come from one place", got)
	}
	if reqs := pool.requests(); len(reqs) != 1 || !strings.HasSuffix(reqs[0], ".dsc") {
		t.Errorf("pool was asked %v; after its .dsc 404 nothing else should go there", reqs)
	}
}

func sha1hex(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// snapshotFiles builds what snapshot.debian.org serves for one version: the
// file listing keyed by sha1, and each file under /file/<sha1>.
func snapshotFiles(t *testing.T, src, ver string, files map[string]string) map[string]string {
	t.Helper()
	info := map[string][]map[string]string{}
	out := map[string]string{}
	for name, body := range files {
		h := sha1hex(body)
		info[h] = append(info[h], map[string]string{"name": name, "archive_name": "debian"})
		out["/file/"+h] = body
	}
	listing, err := json.Marshal(map[string]any{"fileinfo": info})
	if err != nil {
		t.Fatal(err)
	}
	out["/mr/package/"+src+"/"+ver+"/srcfiles?fileinfo=1"] = string(listing)
	return out
}

const glibcDsc = `Format: 3.0 (quilt)
Source: glibc
Files:
 aaaa 5 glibc_2.41.orig.tar.xz
 bbbb 5 glibc_2.41-10.debian.tar.xz
`

var dropped = unpackRequest{Source: "glibc", Version: "2.41-10", Component: "main"}

// deb.debian.org no longer has glibc 2.41-10; snapshot.debian.org has every
// version, addressed by sha1.
func TestSnapshotFallbackWhenThePoolDroppedTheVersion(t *testing.T) {
	pool := newArchiveServer(t, nil)
	snap := newArchiveServer(t, snapshotFiles(t, "glibc", "2.41-10", map[string]string{
		"glibc_2.41-10.dsc":           glibcDsc,
		"glibc_2.41.orig.tar.xz":      "orig1",
		"glibc_2.41-10.debian.tar.xz": "debia",
	}))
	s := &server{mirror: pool.URL, fallback: snapshot{base: snap.URL}, hc: http.DefaultClient}

	work := t.TempDir()
	if _, err := s.fetchSources(context.Background(), dropped, work); err != nil {
		t.Fatalf("fetchSources: %v", err)
	}
	for name, want := range map[string]string{
		"glibc_2.41-10.dsc": glibcDsc, "glibc_2.41.orig.tar.xz": "orig1", "glibc_2.41-10.debian.tar.xz": "debia",
	} {
		if got := read(t, filepath.Join(work, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The address is the hash, so a body that does not match it is refused.
// dpkg-source runs with --no-check; this is the only integrity check on that path.
func TestSnapshotRejectsAFileThatDoesNotMatchItsHash(t *testing.T) {
	files := snapshotFiles(t, "glibc", "2.41-10", map[string]string{
		"glibc_2.41-10.dsc":           glibcDsc,
		"glibc_2.41.orig.tar.xz":      "orig1",
		"glibc_2.41-10.debian.tar.xz": "debia",
	})
	files["/file/"+sha1hex("orig1")] = "tampered"
	snap := newArchiveServer(t, files)
	s := &server{mirror: newArchiveServer(t, nil).URL, fallback: snapshot{base: snap.URL}, hc: http.DefaultClient}

	_, err := s.fetchSources(context.Background(), dropped, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "sha1") {
		t.Fatalf("err = %v, want a sha1 mismatch", err)
	}
}

// Only a 404 says "not here". A 503 says the mirror is unwell, and answering it
// by moving load onto an archive that is not a mirror would hide the outage.
func TestNoFallbackOnServerError(t *testing.T) {
	pool := newArchiveServer(t, nil)
	pool.status = http.StatusServiceUnavailable
	lp := newArchiveServer(t, nil)
	s := &server{mirror: pool.URL, fallback: launchpad{base: lp.URL}, hc: http.DefaultClient}

	if _, err := s.fetchSources(context.Background(), sru, t.TempDir()); err == nil {
		t.Fatal("expected the pool's 503 to be an error")
	}
	if reqs := lp.requests(); len(reqs) != 0 {
		t.Errorf("fallback was asked %v after a 503", reqs)
	}
}

// When the pool has the package, the fallback is never asked.
func TestPoolIsPreferred(t *testing.T) {
	p := "/pool/main/c/coreutils/"
	pool := newArchiveServer(t, map[string]string{
		p + "coreutils_8.32-4.1ubuntu1.2.dsc":           testDsc,
		p + "coreutils_8.32.orig.tar.xz":                "orig1",
		p + "coreutils_8.32-4.1ubuntu1.2.debian.tar.xz": "debia",
	})
	lp := newArchiveServer(t, nil)
	s := &server{mirror: pool.URL, fallback: launchpad{base: lp.URL}, hc: http.DefaultClient}

	if _, err := s.fetchSources(context.Background(), sru, t.TempDir()); err != nil {
		t.Fatalf("fetchSources: %v", err)
	}
	if reqs := lp.requests(); len(reqs) != 0 {
		t.Errorf("fallback was asked %v although the pool had everything", reqs)
	}
}

// Without a fallback a 404 stays a 404, and still reads as one.
func TestWithoutFallbackTheMissIsReported(t *testing.T) {
	s := &server{mirror: newArchiveServer(t, nil).URL, hc: http.DefaultClient}
	_, err := s.fetchSources(context.Background(), sru, t.TempDir())
	if !errors.Is(err, errNotFound) {
		t.Fatalf("err = %v, want errNotFound", err)
	}
}

// A typo in FALLBACK must not silently mean "no fallback".
func TestFallbackFromEnv(t *testing.T) {
	for name, want := range map[string]string{"": "none", "launchpad": "launchpad", "snapshot": "snapshot"} {
		a, err := fallbackFromEnv(name, "")
		if err != nil || fallbackName(a) != want {
			t.Errorf("%q -> %s, %v; want %s", name, fallbackName(a), err, want)
		}
	}
	if _, err := fallbackFromEnv("snapshots", ""); err == nil {
		t.Error("an unknown name was accepted")
	}
}

// Whose failure it was decides the status: 404 for "nowhere", 502 when the pool
// or the fallback failed, 500 when this service did. A 502 for our own failure
// would send whoever reads the logs to look at Debian instead of this host.
func TestUnpackStatusSaysWhoseFailureItWas(t *testing.T) {
	broken := newArchiveServer(t, nil)
	broken.status = http.StatusServiceUnavailable

	// A root that is a file: MkdirAll fails before anything is fetched.
	fileRoot := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileRoot, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		s    *server
		want int
	}{
		"nowhere":       {&server{root: t.TempDir(), mirror: newArchiveServer(t, nil).URL, hc: http.DefaultClient}, http.StatusNotFound},
		"mirror is 503": {&server{root: t.TempDir(), mirror: broken.URL, hc: http.DefaultClient}, http.StatusBadGateway},
		"our disk":      {&server{root: fileRoot, mirror: newArchiveServer(t, nil).URL, hc: http.DefaultClient}, http.StatusInternalServerError},
	} {
		_, err := tc.s.unpack(context.Background(), sru)
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if got := unpackStatus(err); got != tc.want {
			t.Errorf("%s: status %d, want %d (err: %v)", name, got, tc.want, err)
		}
	}
}

// Launchpad's librarian serves a .diff.gz with Content-Encoding: gzip. Go's
// transport, left to choose Accept-Encoding itself, decodes that away and the
// file on disk is a plain diff - linux-hwe-7.0 7.0.0-38.38~24.04.4 then failed
// in dpkg-source with "gunzip: not in gzip format". The file must be stored as
// sent.
func TestDownloadKeepsAGzipFileCompressed(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("--- a\n+++ b\n"))
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip") // regardless of what was asked, as the librarian does
		w.Write(gz.Bytes())
	}))
	defer srv.Close()

	s := &server{hc: &http.Client{}}
	dst := filepath.Join(t.TempDir(), "x.diff.gz")
	if err := s.download(context.Background(), srv.URL+"/x.diff.gz", dst); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dst); got != gz.String() {
		t.Fatalf("stored %d bytes starting %q, want the %d gzip bytes as sent", len(got), got[:min(len(got), 8)], gz.Len())
	}
}
