package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testID = "9851d79b1f32e900a27f8d0a68e29d750303f0c6"

type fakeIndex struct {
	mu      sync.Mutex
	rows    map[string][]entry
	err     error
	lookups atomic.Int64
}

func (f *fakeIndex) Lookup(_ context.Context, source, id string) ([]entry, error) {
	f.lookups.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[source+"/"+id], nil
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Deterministic order, with the wanted member last, so a reader that
	// stops early is caught.
	for _, name := range []string{"python/lib/os.py", "python/bin/python3.14"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type fixture struct {
	idx     *fakeIndex
	proxy   *httptest.Server // this service
	mirror  *httptest.Server
	routes  map[string]http.HandlerFunc
	routeMu sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{idx: &fakeIndex{rows: map[string][]entry{}}, routes: map[string]http.HandlerFunc{}}
	f.mirror = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.routeMu.Lock()
		h, ok := f.routes[r.URL.Path]
		f.routeMu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.mirror.Close)
	s := newServer(f.idx, &http.Client{})
	f.proxy = httptest.NewServer(s.routes())
	t.Cleanup(f.proxy.Close)
	return f
}

func (f *fixture) serveBytes(path string, body []byte) {
	f.routeMu.Lock()
	defer f.routeMu.Unlock()
	f.routes[path] = func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }
}

func (f *fixture) handle(path string, h http.HandlerFunc) {
	f.routeMu.Lock()
	defer f.routeMu.Unlock()
	f.routes[path] = h
}

func (f *fixture) pythonEntry(path string, size int) entry {
	return entry{Kind: "executable", URL: f.mirror.URL + path, Format: "tar.gz",
		Archive: "cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-install_only.tar.gz",
		Member:  "python/bin/python3.14", Size: uint64(size)}
}

func get(t *testing.T, url string) (*http.Response, []byte, error) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

func TestServesMemberWithLengthAndHeaders(t *testing.T) {
	f := newFixture(t)
	f.serveBytes("/a.tar.gz", tarGz(t, map[string]string{"python/lib/os.py": "import sys", "python/bin/python3.14": "\x7fELF-python"}))
	f.idx.rows["pbs/"+testID] = []entry{f.pythonEntry("/a.tar.gz", 11)}

	for _, ep := range []string{"debuginfo", "executable"} {
		resp, body, err := get(t, f.proxy.URL+"/pbs/buildid/"+testID+"/"+ep)
		if err != nil || resp.StatusCode != 200 || string(body) != "\x7fELF-python" {
			t.Fatalf("%s: %v %d %q", ep, err, resp.StatusCode, body)
		}
		if resp.ContentLength != 11 {
			t.Errorf("%s: content-length %d, want 11 so truncation is detectable", ep, resp.ContentLength)
		}
		if got := resp.Header.Get("X-Debuginfod-File"); got != "/python/bin/python3.14" {
			t.Errorf("%s: x-debuginfod-file %q", ep, got)
		}
		if resp.Header.Get("X-Status") != "" {
			t.Errorf("%s: x-status leaked into the final response", ep)
		}
	}
}

// TestProbeIsAnsweredBeforeTheArchiveArrives is the reason this service sends
// Early Hints: cmd/proxy gives every upstream 5 s for headers, and the member
// may sit 100 MB into an archive. The mirror here never answers at all.
func TestProbeIsAnsweredBeforeTheArchiveArrives(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.handle("/slow.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	f.idx.rows["pbs/"+testID] = []entry{f.pythonEntry("/slow.tar.gz", 116<<20)}

	errSatisfied := errors.New("satisfied")
	var hint textproto.MIMEHeader
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints && h.Get("X-Status") == "200" {
				hint = h
				return errSatisfied
			}
			return nil
		},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.proxy.URL+"/pbs/buildid/"+testID+"/debuginfo", nil)
	_, err := http.DefaultClient.Do(req)
	if hint == nil {
		t.Fatalf("no Early Hints before the deadline: %v", err)
	}
	if hint.Get("X-Debuginfod-Size") != fmt.Sprint(116<<20) || !strings.HasPrefix(hint.Get("X-Debuginfod-Archive"), "cpython-") {
		t.Fatalf("hint does not describe the artifact: %v", hint)
	}
}

func TestSeparateDebugFileIsNeverAnExecutable(t *testing.T) {
	f := newFixture(t)
	// The archive is there, so only the kind can be what refuses it.
	f.serveBytes("/a.tar.gz", tarGz(t, map[string]string{"python/bin/python3.14": "dbg"}))
	e := f.pythonEntry("/a.tar.gz", 3)
	e.Kind = "debuginfo"
	f.idx.rows["ubuntu/"+testID] = []entry{e}
	resp, _, _ := get(t, f.proxy.URL+"/ubuntu/buildid/"+testID+"/executable")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
}

func TestFallsBackWhenThePoolDroppedTheVersion(t *testing.T) {
	f := newFixture(t)
	f.serveBytes("/launchpad/a.tar.gz", tarGz(t, map[string]string{"python/bin/python3.14": "from-launchpad"}))
	pool := f.pythonEntry("/pool/a.tar.gz", 14) // not served: 404
	lp := f.pythonEntry("/launchpad/a.tar.gz", 14)
	// Launchpad first in the index, to prove the pool is still tried first.
	f.idx.rows["pbs/"+testID] = []entry{lp, pool}

	var poolHits atomic.Int64
	f.handle("/pool/a.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		poolHits.Add(1)
		http.NotFound(w, r)
	})
	resp, body, _ := get(t, f.proxy.URL+"/pbs/buildid/"+testID+"/debuginfo")
	if resp.StatusCode != 200 || string(body) != "from-launchpad" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if poolHits.Load() != 1 {
		t.Fatalf("pool asked %d times, want 1", poolHits.Load())
	}
}

func TestFailureStatuses(t *testing.T) {
	f := newFixture(t)
	f.handle("/broken.tar.gz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	f.serveBytes("/other.tar.gz", tarGz(t, map[string]string{"python/lib/os.py": "x"}))

	for _, c := range []struct {
		path string
		want int
	}{
		{"/missing.tar.gz", http.StatusNotFound},          // every copy gone
		{"/broken.tar.gz", http.StatusBadGateway},         // the mirror failed
		{"/other.tar.gz", http.StatusInternalServerError}, // our index is wrong
	} {
		f.idx.rows["pbs/"+testID] = []entry{f.pythonEntry(c.path, 1)}
		f.lookupReset(t)
		resp, _, _ := get(t, f.proxy.URL+"/pbs/buildid/"+testID+"/debuginfo")
		if resp.StatusCode != c.want {
			t.Errorf("%s: got %d, want %d", c.path, resp.StatusCode, c.want)
		}
		if resp.Header.Get("X-Debuginfod-Archive") != "" {
			t.Errorf("%s: an error response carries x-debuginfod-archive", c.path)
		}
	}
}

// lookupReset rebuilds the service so the lookup cache does not answer from
// the previous case.
func (f *fixture) lookupReset(t *testing.T) {
	f.proxy.Config.Handler = newServer(f.idx, &http.Client{}).routes()
}

// TestTruncatedArchiveBreaksTheConnection: once 200 and a length are out, a
// failure must not look like a complete file.
func TestTruncatedArchiveBreaksTheConnection(t *testing.T) {
	f := newFixture(t)
	full := tarGz(t, map[string]string{"python/bin/python3.14": strings.Repeat("A", 1<<20)})
	f.serveBytes("/cut.tar.gz", full[:len(full)/2])
	f.idx.rows["pbs/"+testID] = []entry{f.pythonEntry("/cut.tar.gz", 1<<20)}
	resp, _, err := get(t, f.proxy.URL+"/pbs/buildid/"+testID+"/debuginfo")
	if resp.StatusCode == 200 && err == nil {
		t.Fatal("a truncated archive was served as a complete 200")
	}
}

func TestMissesAreCachedErrorsAreNot(t *testing.T) {
	f := newFixture(t)
	url := f.proxy.URL + "/ubuntu/buildid/" + testID + "/debuginfo"
	for range 3 {
		get(t, url)
	}
	if n := f.idx.lookups.Load(); n != 1 {
		t.Fatalf("a miss was looked up %d times, want 1", n)
	}

	f.lookupReset(t)
	f.idx.lookups.Store(0)
	f.idx.err = errors.New("clickhouse down")
	for range 3 {
		resp, _, _ := get(t, url)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("lookup failure answered %d", resp.StatusCode)
		}
	}
	if n := f.idx.lookups.Load(); n != 3 {
		t.Fatalf("a failed lookup was cached: %d lookups, want 3", n)
	}
}

func TestRoutes(t *testing.T) {
	f := newFixture(t)
	for path, want := range map[string]int{
		"/ubuntu/buildid/" + testID + "/source/usr/src/glibc/elf/rtld.c": http.StatusNotImplemented,
		"/debian/buildid/" + testID + "/debuginfo":                       http.StatusNotFound,
		"/pbs/buildid/" + strings.ToUpper(testID) + "/debuginfo":         http.StatusNotFound,
		"/pbs/buildid/" + testID + "/whatever":                           http.StatusNotFound,
		"/status":                                                        http.StatusOK,
	} {
		resp, _, _ := get(t, f.proxy.URL+path)
		if resp.StatusCode != want {
			t.Errorf("%s: got %d, want %d", path, resp.StatusCode, want)
		}
	}
	if n := f.idx.lookups.Load(); n != 0 {
		t.Fatalf("%d lookups for requests that cannot be in the index", n)
	}
}
