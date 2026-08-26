package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/julienschmidt/httprouter"
)

// stepServer releases the body in chunks: the first goes out immediately, each
// subsequent one only after a token from the release channel. It lets a client be
// positioned exactly mid-download without relying on sleeps.
type stepServer struct {
	*httptest.Server
	release chan struct{}
	hits    atomic.Int64
	payload []byte
}

func newStepServer(t *testing.T, chunks, chunkSize int) *stepServer {
	t.Helper()
	s := &stepServer{
		release: make(chan struct{}, chunks),
		payload: make([]byte, chunks*chunkSize),
	}
	for i := range s.payload {
		s.payload[i] = byte(i % 251)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		w.Header().Set("content-type", "application/octet-stream")
		w.Header().Set("x-debuginfod-size", fmt.Sprintf("%d", len(s.payload)))
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			if i > 0 {
				<-s.release
			}
			w.Write(s.payload[i*chunkSize : (i+1)*chunkSize])
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// releaseAll lets every remaining chunk out.
func (s *stepServer) releaseAll(chunks int) {
	for i := 0; i < chunks; i++ {
		s.release <- struct{}{}
	}
}

// Mirrors the finder's client: DisableCompression stops Go from adding its own
// gzip or silently decompressing the response. The whole cache mechanism rests on
// compressed bytes reaching us untouched - with http.DefaultClient the test would
// sail past the real path.
var testFetchClient = &http.Client{Transport: &http.Transport{DisableCompression: true}}

func fetchFrom(url string) fetchFn {
	return func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept-encoding", upstreamAcceptEncoding)
		return testFetchClient.Do(req)
	}
}

// bodyOf returns the response body, decompressing it if it came gzipped.
// After normalisation on ingest the cache always holds gzip, so a client that
// accepts gzip receives compressed bytes.
func bodyOf(t *testing.T, w *gateWriter) []byte {
	t.Helper()
	raw := w.body()
	if w.hdr.Get("content-encoding") != "gzip" {
		return raw
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("odpowiedz oznaczona jako gzip nie jest poprawnym gzipem: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("rozpakowanie odpowiedzi: %v", err)
	}
	return out
}

func newTestCache(t *testing.T, maxBytes int64) *fileCache {
	t.Helper()
	c, err := newFileCache(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// One blocked client must not hold up the others. This is exactly the failure mode
// that disqualifies io.MultiWriter: it writes sequentially, so the slowest receiver
// stalls the download and everyone else with it.
func TestFileCacheSlowClientDoesNotBlockOthers(t *testing.T) {
	const chunks, chunkSize = 4, 32 << 10
	up := newStepServer(t, chunks, chunkSize)
	c := newTestCache(t, 0)

	gate := make(chan struct{})
	slow := newGateWriter(gate)
	fast := make([]*gateWriter, 5)

	var slowWG, fastWG sync.WaitGroup
	slowWG.Add(1)
	go func() {
		defer slowWG.Done()
		if err := c.Serve(context.Background(), "k", slow, "up", fetchFrom(up.URL)); err != nil {
			t.Errorf("wolny klient: %v", err)
		}
	}()

	// The slow one must already be sitting on the gate before the fast ones start -
	// otherwise the test could pass by accident, with the slow one blocking nothing
	// yet.
	waitFor(t, 2*time.Second, func() bool {
		slow.mu.Lock()
		defer slow.mu.Unlock()
		return slow.passed
	}, "wolny klient dotarl do bramki")

	for i := range fast {
		fast[i] = newGateWriter(nil)
		fastWG.Add(1)
		go func(i int) {
			defer fastWG.Done()
			if err := c.Serve(context.Background(), "k", fast[i], "up", fetchFrom(up.URL)); err != nil {
				t.Errorf("szybki %d: %v", i, err)
			}
		}(i)
	}
	up.releaseAll(chunks - 1)

	// With head-of-line blocking present, the fast clients would never finish.
	waitGroupDone(t, &fastWG, 5*time.Second,
		"szybcy klienci nie skonczyli, gdy wolny stoi - head-of-line blocking")

	close(gate)
	waitGroupDone(t, &slowWG, 5*time.Second, "wolny klient nie skonczyl")

	for i, w := range fast {
		if got := bodyOf(t, w); !bytes.Equal(got, up.payload) {
			t.Errorf("szybki %d: %d bajtow, oczekiwano %d", i, len(got), len(up.payload))
		}
	}
	if got := bodyOf(t, slow); !bytes.Equal(got, up.payload) {
		t.Errorf("wolny: %d bajtow, oczekiwano %d", len(got), len(up.payload))
	}
	if got := up.hits.Load(); got != 1 {
		t.Errorf("upstream odpytany %d razy, oczekiwano 1", got)
	}
}

// A client joining mid-download receives the whole file, including what went out
// before it arrived.
func TestFileCacheLateJoinerGetsWholeFile(t *testing.T) {
	const chunks, chunkSize = 6, 16 << 10
	up := newStepServer(t, chunks, chunkSize)
	c := newTestCache(t, 0)

	leader := newGateWriter(nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Serve(context.Background(), "k", leader, "up", fetchFrom(up.URL))
	}()

	// Wait until the leader has some data, and only then join.
	waitFor(t, 2*time.Second, func() bool { return leader.bytesWritten() > 0 },
		"lider zapisal pierwsza porcje")

	joiner := newGateWriter(nil)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.Serve(context.Background(), "k", joiner, "up", fetchFrom(up.URL)); err != nil {
			t.Errorf("spozniony: %v", err)
		}
	}()

	up.releaseAll(chunks - 1)
	waitGroupDone(t, &wg, 5*time.Second, "klienci nie skonczyli")

	if got := bodyOf(t, joiner); !bytes.Equal(got, up.payload) {
		t.Errorf("spozniony dostal %d bajtow, oczekiwano %d", len(got), len(up.payload))
	}
	if got := up.hits.Load(); got != 1 {
		t.Errorf("upstream odpytany %d razy, oczekiwano 1", got)
	}
}

// Once the download has finished, further requests come from disk with no network traffic.
func TestFileCacheHitFromDisk(t *testing.T) {
	const chunks, chunkSize = 2, 8 << 10
	up := newStepServer(t, chunks, chunkSize)
	up.releaseAll(chunks - 1)
	c := newTestCache(t, 0)

	first := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", first, "up", fetchFrom(up.URL)); err != nil {
		t.Fatalf("pierwsze zapytanie: %v", err)
	}
	second := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", second, "up", fetchFrom(up.URL)); err != nil {
		t.Fatalf("drugie zapytanie: %v", err)
	}

	if first.hdr.Get("x-cache") != "MISS" {
		t.Errorf("pierwsze zapytanie: x-cache=%q, oczekiwano MISS", first.hdr.Get("x-cache"))
	}
	if second.hdr.Get("x-cache") != "HIT" {
		t.Errorf("drugie zapytanie: x-cache=%q, oczekiwano HIT", second.hdr.Get("x-cache"))
	}
	if !bytes.Equal(bodyOf(t, second), up.payload) {
		t.Error("HIT zwrocil inne bajty niz MISS")
	}
	// content-length must describe what ACTUALLY went to the client. After
	// compression on ingest, naively copying the upstream header would put the
	// uncompressed size here and the response would look truncated.
	if want := fmt.Sprintf("%d", len(second.body())); second.hdr.Get("content-length") != want {
		t.Errorf("content-length=%q, a wyslano %s bajtow", second.hdr.Get("content-length"), want)
	}
	// The debuginfod headers must survive being written to the cache.
	if second.hdr.Get("x-debuginfod-size") != fmt.Sprintf("%d", len(up.payload)) {
		t.Errorf("x-debuginfod-size zgubiony na HIT: %q", second.hdr.Get("x-debuginfod-size"))
	}
	if got := up.hits.Load(); got != 1 {
		t.Errorf("upstream odpytany %d razy, oczekiwano 1", got)
	}
}

// The leader disconnecting must not abort the download for everyone else - which is
// why the download runs in the background, detached from any single client's
// context.
func TestFileCacheLeaderDisconnectDoesNotKillDownload(t *testing.T) {
	const chunks, chunkSize = 6, 16 << 10
	up := newStepServer(t, chunks, chunkSize)
	c := newTestCache(t, 0)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := newGateWriter(nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Serve(leaderCtx, "k", leader, "up", fetchFrom(up.URL))
	}()
	waitFor(t, 2*time.Second, func() bool { return leader.bytesWritten() > 0 }, "lider ruszyl")

	follower := newGateWriter(nil)
	var followerErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		followerErr = c.Serve(context.Background(), "k", follower, "up", fetchFrom(up.URL))
	}()
	waitFor(t, 2*time.Second, func() bool { return follower.bytesWritten() > 0 }, "follower ruszyl")

	cancelLeader()
	up.releaseAll(chunks - 1)
	waitGroupDone(t, &wg, 5*time.Second, "klienci nie skonczyli")

	if followerErr != nil {
		t.Errorf("follower dostal blad po rozlaczeniu lidera: %v", followerErr)
	}
	if got := bodyOf(t, follower); !bytes.Equal(got, up.payload) {
		t.Errorf("follower dostal %d bajtow, oczekiwano %d", len(got), len(up.payload))
	}
}

// A broken download must not land in the cache - otherwise the truncated file
// would be served until eviction.
func TestFileCacheTruncatedDownloadNotCached(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-length", "100000")
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 1000))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drops the connection halfway
	}))
	defer up.Close()

	dir := t.TempDir()
	c, err := newFileCache(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	w := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", w, "up", fetchFrom(up.URL)); err == nil {
		t.Error("oczekiwano bledu przy urwanym pobraniu")
	}

	var leftovers []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			leftovers = append(leftovers, filepath.Base(p))
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Errorf("urwane pobranie zostawilo pliki w cache: %v", leftovers)
	}
}

// The key must identify the ARTIFACT, not the request. It used to be the raw
// r.RequestURI, so "?n=1", "?n=2"... minted fresh keys - each with its own upstream
// fetch and its own blob. This guards against that regression.
func TestCacheKeyIdentifiesArtifactNotRequest(t *testing.T) {
	const id = "aabbccddeeff00112233445566778899"

	if cacheKey("debuginfo", id, "") != cacheKey("debuginfo", id, "") {
		t.Error("klucz niestabilny dla tych samych danych wejsciowych")
	}
	// Same build ID, different artifact.
	if cacheKey("debuginfo", id, "") == cacheKey("executable", id, "") {
		t.Error("debuginfo i executable dziela klucz")
	}
	if cacheKey("debuginfo", id, "") == cacheKey("debuginfo", "ffee"+id[4:], "") {
		t.Error("rozne buildid dziela klucz")
	}
	// extraPath exists so that adding "source" to cacheableEndpoints would not
	// collapse every source file of one build ID under a single key.
	if cacheKey("source", id, "/usr/src/a.c") == cacheKey("source", id, "/usr/src/b.c") {
		t.Error("rozne pliki zrodlowe dziela klucz")
	}
}

// End to end through proxyRequest: a query string must not multiply fetches or blobs.
func TestProxyRequestIgnoresQueryStringForCaching(t *testing.T) {
	store := newFakeStore()
	store.put(BuildIDState{BuildID: testBuildID, LastSuccess: true, LastHost: "up"})

	var hits atomic.Int64
	payload := bytes.Repeat([]byte("debuginfo-"), 500)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(payload)
	}))
	defer up.Close()

	f := NewDebugInfoFinder(store)
	f.servers = map[string]*Server{"up": {Name: "up", URL: up.URL, SourceAvailable: 1}}

	c := newTestCache(t, 0)
	s := &serverSrv{finder: f, cache: c}
	handler := AccessLogMiddleware(&fakeAccessLog{}, "debuginfo", s.proxyRequest)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, httprouter.Params{httprouter.Param{Key: "buildid", Value: testBuildID}})
	}))
	defer srv.Close()

	for _, q := range []string{"", "?n=1", "?n=2", "?cache=bust", "?n=1&m=2"} {
		resp, err := http.Get(srv.URL + "/buildid/" + testBuildID + "/debuginfo" + q)
		if err != nil {
			t.Fatalf("q=%q: %v", q, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("q=%q: status %d", q, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Fatalf("q=%q: pusta odpowiedz", q)
		}
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("5 wariantow query -> %d pobran z upstreamu, oczekiwano 1", got)
	}

	var blobs int
	filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.HasSuffix(path, ".meta") {
			blobs++
		}
		return nil
	})
	if blobs != 1 {
		t.Errorf("na dysku %d blobow, oczekiwano 1", blobs)
	}
}

// blobPath must assume nothing about the key's length or characters - a short key
// used to panic, and a key containing a slash would risk escaping the cache
// directory.
func TestBlobPathHandlesAnyKey(t *testing.T) {
	c := newTestCache(t, 0)
	for _, key := range []string{"", "a", "ab", "abc", "gzip\x00/buildid/x/debuginfo", "../../etc/passwd"} {
		path := c.blobPath(key)
		rel, err := filepath.Rel(c.dir, path)
		if err != nil || filepath.IsAbs(rel) || len(rel) > 0 && rel[0] == '.' {
			t.Errorf("klucz %q daje sciezke poza cache: %s", key, path)
		}
	}
}

// Eviction deletes the least recently used until it drops below the limit.
func TestFileCacheEvictionRemovesOldest(t *testing.T) {
	c := newTestCache(t, 3000)

	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("klucz-%d", i)
		blob := c.blobPath(key)
		if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blob, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blob+".meta", []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(time.Duration(i-10) * time.Hour) // the smaller i, the older
		os.Chtimes(blob, ts, ts)
	}

	if err := c.evictOnce(); err != nil {
		t.Fatal(err)
	}

	var kept []int
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(c.blobPath(fmt.Sprintf("klucz-%d", i))); err == nil {
			kept = append(kept, i)
		}
	}
	if len(kept) > 3 {
		t.Errorf("eviction nie zszedl do limitu, zostalo %d plikow (%v)", len(kept), kept)
	}
	for _, i := range kept {
		if i < 2 {
			t.Errorf("zostal stary plik %d, a nowsze skasowano (zostalo %v)", i, kept)
		}
	}
	// The .meta must disappear with its blob, or the directory fills with orphans.
	for i := 0; i < 5-len(kept); i++ {
		if _, err := os.Stat(c.blobPath(fmt.Sprintf("klucz-%d", i)) + ".meta"); err == nil {
			t.Errorf("osierocony .meta po skasowanym blobie %d", i)
		}
	}
}

// Eviction is disabled when maxBytes <= 0.
func TestFileCacheEvictionDisabled(t *testing.T) {
	c := newTestCache(t, 0)
	blob := c.blobPath("k")
	os.MkdirAll(filepath.Dir(blob), 0o755)
	os.WriteFile(blob, make([]byte, 10000), 0o644)

	if err := c.evictOnce(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Error("eviction skasowal plik mimo wylaczonego limitu")
	}
}

// gzipServer returns gzip-compressed content, the way upstreams do once they see
// our Accept-Encoding: gzip.
func newGzipServer(t *testing.T, payload []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	hits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("accept-encoding"); got != "" && got != upstreamAcceptEncoding {
			t.Errorf("upstream dostal accept-encoding=%q, oczekiwano %q", got, upstreamAcceptEncoding)
		}
		w.Header().Set("content-encoding", "gzip")
		w.Header().Set("content-type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(compressed.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// With content negotiation removed, the cache holds gzip and it goes out exactly
// that way to EVERY client, including one that never advertised gzip. All traffic
// arrives via Cloudflare, which settles the encoding with the end client itself.
func TestFileCacheAlwaysServesGzipVerbatim(t *testing.T) {
	payload := bytes.Repeat([]byte("debuginfo-"), 5000)
	up, hits := newGzipServer(t, payload)
	c := newTestCache(t, 0)

	w := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", w, "up", fetchFrom(up.URL)); err != nil {
		t.Fatal(err)
	}
	if w.hdr.Get("content-encoding") != "gzip" {
		t.Errorf("content-encoding=%q, oczekiwano gzip", w.hdr.Get("content-encoding"))
	}
	// Note: the upstream already returns gzip, so compressOnIngest is false and our
	// compressor does NOT run here - the blob is the upstream's bytes copied
	// verbatim. klauspost -> compress/gzip interoperability is guarded by
	// TestFileCacheCompressesIdentityOnIngest, where the upstream returns identity.
	if got := bodyOf(t, w); !bytes.Equal(got, payload) {
		t.Errorf("po rozpakowaniu %d B, oczekiwano %d B", len(got), len(payload))
	}

	w2 := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", w2, "up", fetchFrom(up.URL)); err != nil {
		t.Fatal(err)
	}
	if w2.hdr.Get("x-cache") != "HIT" {
		t.Errorf("drugi klient: x-cache=%q, oczekiwano HIT", w2.hdr.Get("x-cache"))
	}
	// On a HIT from disk the size is known, so content-length MUST be present -
	// the transcode branch used to delete it and the response went out chunked.
	if want := fmt.Sprintf("%d", len(w2.body())); w2.hdr.Get("content-length") != want {
		t.Errorf("content-length=%q, a wyslano %s B", w2.hdr.Get("content-length"), want)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream odpytany %d razy, oczekiwano 1", hits.Load())
	}
}

// A client advertising gzip gets the stored bytes verbatim - zero CPU work.
func TestFileCacheServesGzipVerbatimWhenAccepted(t *testing.T) {
	payload := bytes.Repeat([]byte("debuginfo-"), 5000)
	up, _ := newGzipServer(t, payload)
	c := newTestCache(t, 0)

	w := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", w, "up", fetchFrom(up.URL)); err != nil {
		t.Fatal(err)
	}
	if w.hdr.Get("content-encoding") != "gzip" {
		t.Errorf("content-encoding=%q, oczekiwano gzip", w.hdr.Get("content-encoding"))
	}
	// The body must still be compressed, i.e. smaller than the original.
	if len(w.body()) >= len(payload) {
		t.Errorf("odpowiedz ma %d B, spodziewano sie skompresowanej (< %d B)", len(w.body()), len(payload))
	}
	zr, err := gzip.NewReader(bytes.NewReader(w.body()))
	if err != nil {
		t.Fatalf("odpowiedz nie jest poprawnym gzipem: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("po rozpakowaniu tresc sie nie zgadza (err=%v)", err)
	}
}

// identityServer plays an upstream that IGNORES our Accept-Encoding: gzip and
// answers with uncompressed content - exactly the case compression on ingest
// exists for.
func newIdentityServer(t *testing.T, payload []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/octet-stream")
		w.Header().Set("content-length", fmt.Sprintf("%d", len(payload)))
		w.Header().Set("x-debuginfod-size", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func storedBlobSize(t *testing.T, c *fileCache, key string) int64 {
	t.Helper()
	st, err := os.Stat(c.blobPath(key))
	if err != nil {
		t.Fatalf("brak pliku w cache: %v", err)
	}
	return st.Size()
}

// The upstream returned identity -> gzip must land on disk, and a gzip-capable
// client receives it verbatim. Without this, every further request for that file
// would go out uncompressed for the rest of the entry's life.
func TestFileCacheCompressesIdentityOnIngest(t *testing.T) {
	payload := bytes.Repeat([]byte("debuginfo-payload-"), 4000)
	up, hits := newIdentityServer(t, payload)
	c := newTestCache(t, 0)

	w := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", w, "up", fetchFrom(up.URL)); err != nil {
		t.Fatal(err)
	}

	if w.hdr.Get("content-encoding") != "gzip" {
		t.Errorf("content-encoding=%q, oczekiwano gzip po kompresji przy zapisie",
			w.hdr.Get("content-encoding"))
	}
	if !bytes.Equal(bodyOf(t, w), payload) {
		t.Error("tresc po rozpakowaniu nie zgadza sie z oryginalem")
	}

	stored := storedBlobSize(t, c, "k")
	if stored >= int64(len(payload)) {
		t.Errorf("na dysku %d B, oczekiwano mniej niz %d B (plik nieskompresowany)",
			stored, len(payload))
	}
	t.Logf("upstream oddal %d B identity -> na dysku %d B gzip (%.1fx)",
		len(payload), stored, float64(len(payload))/float64(stored))

	// The second request comes from disk and is still valid gzip.
	w2 := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", w2, "up", fetchFrom(up.URL)); err != nil {
		t.Fatal(err)
	}
	if w2.hdr.Get("x-cache") != "HIT" || !bytes.Equal(bodyOf(t, w2), payload) {
		t.Errorf("HIT po kompresji przy zapisie zepsuty: x-cache=%q", w2.hdr.Get("x-cache"))
	}
	if hits.Load() != 1 {
		t.Errorf("upstream odpytany %d razy, oczekiwano 1", hits.Load())
	}
}

// The nastiest bug on this path: the upstream's content-length describes the
// UNCOMPRESSED body. Copied verbatim alongside gzip bytes it yields a response the
// client will consider truncated.
func TestFileCacheDropsStaleContentLengthAfterIngestCompression(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 60000)
	up, _ := newIdentityServer(t, payload)
	c := newTestCache(t, 0)

	w := newGateWriter(nil)
	if err := c.Serve(context.Background(), "k", w, "up", fetchFrom(up.URL)); err != nil {
		t.Fatal(err)
	}

	cl := w.hdr.Get("content-length")
	if cl == fmt.Sprintf("%d", len(payload)) {
		t.Fatalf("content-length=%s to rozmiar SPRZED kompresji - klient zobaczy ucieta odpowiedz", cl)
	}
	if want := fmt.Sprintf("%d", len(w.body())); cl != "" && cl != want {
		t.Errorf("content-length=%q, a wyslano %s bajtow", cl, want)
	}
}

// Compression on ingest must not break streaming: gzip.Writer buffers internally,
// so without a flush followers would stand still despite data arriving, and the
// gzip footer has to reach them before the download is marked done.
func TestFileCacheIngestCompressionStreamsToFollowers(t *testing.T) {
	const chunks, chunkSize = 6, 16 << 10
	up := newStepServer(t, chunks, chunkSize)
	c := newTestCache(t, 0)

	leader := newGateWriter(nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Serve(context.Background(), "k", leader, "up", fetchFrom(up.URL))
	}()
	waitFor(t, 2*time.Second, func() bool { return leader.bytesWritten() > 0 },
		"lider dostal dane mimo buforowania w gzip.Writer")

	follower := newGateWriter(nil)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.Serve(context.Background(), "k", follower, "up", fetchFrom(up.URL)); err != nil {
			t.Errorf("follower: %v", err)
		}
	}()

	up.releaseAll(chunks - 1)
	waitGroupDone(t, &wg, 5*time.Second, "klienci nie skonczyli")

	// bodyOf decompresses - if the gzip footer never arrived, it blows up here.
	if got := bodyOf(t, follower); !bytes.Equal(got, up.payload) {
		t.Errorf("follower dostal %d bajtow, oczekiwano %d", len(got), len(up.payload))
	}
	if got := bodyOf(t, leader); !bytes.Equal(got, up.payload) {
		t.Errorf("lider dostal %d bajtow, oczekiwano %d", len(got), len(up.payload))
	}
}

// A race over the temp file: a follower joins the inflight entry just before the
// leader finishes downloading and renames. The follower used to open the file BY
// PATH, which no longer existed -> ENOENT -> HTTP 500 for a file sitting complete
// on disk. It now inherits the descriptor, so the rename does not touch it.
//
// The test deliberately uses short downloads and high concurrency to hit the window
// between os.Rename and delete(c.inflight, key).
func TestFileCacheNoRaceBetweenRenameAndFollower(t *testing.T) {
	payload := bytes.Repeat([]byte("debuginfo-"), 200)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer up.Close()

	// 40 rounds are probabilistically enough; 200 took ~60% of the whole suite's
	// runtime, and the suite has to stay fast enough to run on every change.
	const rounds, clients = 40, 24
	type result struct {
		err  error
		body []byte
	}

	for round := 0; round < rounds; round++ {
		c := newTestCache(t, 0)
		key := fmt.Sprintf("klucz-%d", round)

		results := make([]result, clients)
		var wg sync.WaitGroup
		for i := 0; i < clients; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				w := newGateWriter(nil)
				// No t.Fatalf/bodyOf inside the goroutine - FailNow may only be called
				// from the test's own goroutine, and here it would abort result
				// collection exactly when the race comes back.
				if err := c.Serve(context.Background(), key, w, "up", fetchFrom(up.URL)); err != nil {
					results[i] = result{err: err}
					return
				}
				results[i] = result{body: w.body()}
			}(i)
		}
		waitGroupDone(t, &wg, 30*time.Second,
			fmt.Sprintf("runda %d nie skonczyla sie - prawdopodobnie zgubiony Broadcast", round))

		for i, res := range results {
			if res.err != nil {
				t.Fatalf("runda %d, klient %d: %v", round, i, res.err)
			}
			decoded, err := gzip.NewReader(bytes.NewReader(res.body))
			if err != nil {
				t.Fatalf("runda %d, klient %d: odpowiedz nie jest gzipem: %v", round, i, err)
			}
			got, err := io.ReadAll(decoded)
			decoded.Close()
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("runda %d, klient %d: %d bajtow zamiast %d (err=%v)",
					round, i, len(got), len(payload), err)
			}
		}
	}
	t.Logf("%d zadan (%d rund x %d klientow) bez bledu i bez rozjazdu tresci",
		rounds*clients, rounds, clients)
}

// entryReader.Read with a zero-length buffer must not spin. The wait predicate is
// false in that case (the leader is ahead), so without an explicit short-circuit the
// loop burned a full core until the client disconnected.
func TestEntryReaderZeroLengthReadDoesNotSpin(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "blob")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	if _, err := tmp.Write([]byte("jakas tresc")); err != nil {
		t.Fatal(err)
	}

	e := newCacheEntry()
	e.size = 11 // the leader is ahead of the reader
	r := &entryReader{ctx: context.Background(), e: e, f: tmp}

	done := make(chan struct{})
	go func() {
		r.Read(make([]byte, 0))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read(bufor zerowej dlugosci) nie wrocil - petla na pelnym rdzeniu")
	}
}
