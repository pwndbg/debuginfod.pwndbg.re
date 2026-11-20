// Command deb-debuginfod serves /buildid/<id>/source/<path> for Debian build
// IDs, and nothing else. The same binary serves Ubuntu as a second instance,
// pointed at debuginfod.ubuntu.com and archive.ubuntu.com by environment (see
// run.sh); everything below holds for both, with Ubuntu's differences noted
// where they arise.
//
// Why only source: debuginfod.debian.net already serves debuginfo and
// executable well, and cmd/proxy resolves to it. What it does NOT serve is
// sources - every /source/* request returns 404 there, which is why the proxy
// carries SourceAvailable: 0 for debian and answers 501. Measured on
// production, /source succeeds for 1.1% of requests and 1,843 of 3,641 in a
// week were that 501.
//
// How it works, and why it needs no index of its own:
//
//  1. Ask debuginfod.debian.net for the build ID's debuginfo and read only the
//     X-DEBUGINFOD-ARCHIVE header, which names the .deb the file came from.
//     That header is the whole trick - mapping build IDs to packages is
//     otherwise a mirror-sized indexing job.
//  2. Turn that into a source package and version (archive.go).
//  3. Ask cmd/deb-unpack for the unpacked, PATCHED tree. Patched matters: for
//     glibc the Debian series changes 666 files, elf/rtld.c by 97 lines, and
//     rtld.c is a file clients really do request.
//  4. Match the requested path against that tree from the right (srcindex),
//     because DWARF records DW_AT_comp_dir joined with DW_AT_name and that
//     produces things like ./elf/./elf/do-rel.h - a doubled component that
//     normalisation alone resolves to a path which does not exist.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/julienschmidt/httprouter"
	"github.com/pwndbg/debuginfod.pwndbg.re/srcindex"
	"github.com/pwndbg/debuginfod.pwndbg.re/useragent"
	"golang.org/x/sync/singleflight"
)

const (
	// defaultUpstream is asked for headers only; it is the authority on which
	// .deb a build ID belongs to. UPSTREAM_DEBUGINFOD overrides it, which is
	// how the Ubuntu instance points at debuginfod.ubuntu.com.
	defaultUpstream = "https://debuginfod.debian.net"

	// resolveTimeout bounds the header lookup. It is short because the answer
	// is a header, not a payload - if Debian is slow enough to exceed this, the
	// client is better served by a prompt 404 than by a hang, which is exactly
	// what the 46-second aborted requests in production looked like.
	resolveTimeout = 20 * time.Second

	// resolveCacheSize and resolveCacheTTL bound the build ID -> package cache.
	//
	// Without it every source request costs a round trip to Debian, and a
	// debugger stepping through one function asks for dozens of files from the
	// same build ID - measured at ~140 ms per request against ~2 ms once the
	// answer is remembered.
	//
	// Bounded and expiring rather than a plain map: the key is client-supplied,
	// so an unbounded map is a memory leak anyone can trigger. The TTL matters
	// less - a build ID maps to one .deb for ever - but it keeps a wrong answer
	// from an outage or a mirror move from being permanent.
	resolveCacheSize = 10_000
	resolveCacheTTL  = 24 * time.Hour

	// missingTTL and failedTTL bound how long a package that could not be
	// prepared is remembered as such, so a debugger stepping through fifty
	// files of it makes one attempt rather than fifty - each of which is a
	// request to the pool and then to snapshot.debian.org or Launchpad.
	//
	// Two numbers because the two outcomes mean different things. "In neither
	// the pool nor the fallback" is an answer: a version dropped from both does
	// not come back, and the hour only forgives a mirror caught mid-sync. Any
	// other failure - a 5xx, a timeout, dpkg-source - is usually transient, and
	// remembering it for long would turn a blip into an outage, so it is held
	// just long enough to collapse a burst.
	missingTTL = time.Hour
	failedTTL  = time.Minute
)

type srv struct {
	// upstream is the authority on which .deb a build ID belongs to. A field
	// rather than the constant so a test can stand in for it and count how
	// often it is actually asked, and so one binary can serve both Debian and
	// Ubuntu.
	upstream string
	unpacker string
	hc       *http.Client

	mu      sync.Mutex
	indexes map[string]*srcindex.Index
	// lastUsed drives eviction: when each package was last actually served.
	lastUsed map[string]time.Time
	group    singleflight.Group

	resolved *expirable.LRU[string, pkgRef]

	// missing and failed remember packages that could not be prepared, keyed
	// like indexes; see missingTTL. Either may be nil in tests.
	missing *expirable.LRU[string, error]
	failed  *expirable.LRU[string, error]
	// resolveGroup coalesces concurrent lookups of the same cold build ID. A
	// debugger opens several files at once, and without this each one would
	// make its own request to Debian for the identical answer.
	resolveGroup singleflight.Group
}

func main() {
	addr := envOr("LISTEN_ADDR", "127.0.0.1:8036")
	s := &srv{
		upstream: envOr("UPSTREAM_DEBUGINFOD", defaultUpstream),
		unpacker: envOr("UNPACKER_URL", "http://127.0.0.1:8035"),
		indexes:  map[string]*srcindex.Index{},
		lastUsed: map[string]time.Time{},
		resolved: expirable.NewLRU[string, pkgRef](resolveCacheSize, nil, resolveCacheTTL),
		missing:  expirable.NewLRU[string, error](resolveCacheSize, nil, missingTTL),
		failed:   expirable.NewLRU[string, error](resolveCacheSize, nil, failedTTL),
		// No overall timeout: unpacking a source package is minutes on a cold
		// package. Per-request contexts bound the parts that should be quick.
		hc: useragent.Client(&http.Client{}, "deb"),
	}

	router := httprouter.New()
	router.GET("/buildid/:buildid/source/*path", s.handleSource)
	// Answered explicitly rather than left to 404 so a misconfigured caller
	// learns what this service is, instead of concluding the build ID is
	// unknown. cmd/proxy should route only source here.
	router.GET("/buildid/:buildid/debuginfo", s.notImplemented)
	router.GET("/buildid/:buildid/executable", s.notImplemented)
	router.GET("/status", func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}
	budget := envInt("CACHE_MAX_BYTES", 10<<30)
	interval := envDuration("EVICT_INTERVAL", 10*time.Minute)
	go s.EvictLoop(context.Background(), interval, budget)

	log.WithField("addr", addr).WithField("upstream", s.upstream).WithField("unpacker", s.unpacker).
		WithField("cache_max_bytes", budget).Info("deb-debuginfod starting")
	if err := server.ListenAndServe(); err != nil {
		log.WithError(err).Fatal("server failed")
	}
}

func envInt(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		log.WithField("var", k).WithField("value", v).Warn("not a number; using the default")
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.WithField("var", k).WithField("value", v).Warn("not a duration; using the default")
	}
	return def
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *srv) notImplemented(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	http.Error(w, "this backend serves /source/* only; debuginfo and executable come from "+s.upstream,
		http.StatusNotImplemented)
}

func (s *srv) handleSource(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	buildID := ps.ByName("buildid")
	reqPath := ps.ByName("path")
	started := time.Now()
	logger := log.WithField("buildid", buildID).WithField("path", reqPath)

	pkg, err := s.resolve(r.Context(), buildID, r.Header.Get(archiveHeader))
	if err != nil {
		logger.WithError(err).Debug("build ID does not resolve to a package upstream")
		http.Error(w, "unknown build ID", http.StatusNotFound)
		return
	}
	logger = logger.WithField("package", pkg.Source+"_"+pkg.Version)

	key := pkg.Component + "/" + pkg.Source + "_" + pkg.Version
	s.touch(r.Context(), key)

	ix, err := s.indexFor(r.Context(), pkg)
	if errors.Is(err, errSourcesMissing) {
		// An answer, not a failure: 404, which the edge also caches.
		logger.WithError(err).Info("source package is in neither the pool nor the fallback")
		http.Error(w, "source package not found", http.StatusNotFound)
		return
	}
	if err != nil {
		logger.WithError(err).Warn("preparing sources failed")
		http.Error(w, "sources unavailable", prepareStatus(err))
		return
	}

	file := ix.Find(reqPath)
	if file == "" {
		// A miss here is a real answer, not a failure: the path may name a
		// generated file that never existed in the source package.
		logger.Debug("no file in the source tree matches")
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(file)
	if err != nil {
		// Ours: the tree is on our volume, nothing upstream is involved.
		logger.WithError(err).Warn("opening a file the index just offered")
		http.Error(w, "sources unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "sources unavailable", http.StatusInternalServerError)
		return
	}

	// The debuginfod protocol headers, measured rather than assumed: size comes
	// from the file about to be written, and file is the path that was actually
	// matched, so a client can see which candidate won.
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.Header().Set("x-debuginfod-size", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("x-debuginfod-file", file)
	http.ServeContent(w, r, "", info.ModTime(), f)
	logger.WithField("file", file).WithField("took", time.Since(started).Round(time.Millisecond)).
		Info("served source")
}

// archiveHeader is how cmd/proxy passes on the archive it recorded for a build
// ID when it resolved it (see delegateArchiveHeader there).
const archiveHeader = "x-debuginfod-archive"

// resolve finds the package a build ID belongs to.
//
// The proxy's record comes first when it sent one. The upstream drops build IDs
// as versions are superseded, and once it has, asking it is a 404 even though
// the sources may still be in the pool - glibc 2.43-4 was exactly that. The
// record is never worse: it is what the same upstream said when it still knew
// the build ID. A record that does not parse is logged and ignored, falling back
// to the upstream as before; trusting it more than that would make a bug in the
// proxy look like a missing package.
//
// Asking the upstream:
//
// It reads the headers and closes the body immediately: the debuginfo payload
// is megabytes and none of it is wanted. HEAD is not used because debuginfod
// servers are not required to implement it, and a GET whose body is dropped is
// the portable way to ask the same question.
func (s *srv) resolve(ctx context.Context, buildID, recorded string) (pkgRef, error) {
	if pkg, ok := s.resolved.Get(buildID); ok {
		return pkg, nil
	}
	if recorded != "" {
		pkg, err := parseArchive(recorded)
		if err == nil {
			s.resolved.Add(buildID, pkg)
			return pkg, nil
		}
		log.WithError(err).WithField("buildid", buildID).
			Warn("the proxy's recorded archive does not parse; asking upstream instead")
	}
	res, err, _ := s.resolveGroup.Do(buildID, func() (any, error) {
		pkg, err := s.askUpstream(ctx, buildID)
		if err != nil {
			// Deliberately not cached. A negative answer here is usually
			// "upstream was slow" rather than "this build ID does not exist",
			// and remembering it for a day would turn a blip into an outage
			// for that build ID.
			return pkgRef{}, err
		}
		s.resolved.Add(buildID, pkg)
		return pkg, nil
	})
	if err != nil {
		return pkgRef{}, err
	}
	return res.(pkgRef), nil
}

func (s *srv) askUpstream(ctx context.Context, buildID string) (pkgRef, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	url := s.upstream + "/buildid/" + buildID + "/debuginfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return pkgRef{}, err
	}

	resp, err := s.hc.Do(req)
	if err != nil {
		return pkgRef{}, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return pkgRef{}, fmt.Errorf("upstream answered %s", resp.Status)
	}

	archive := resp.Header.Get("x-debuginfod-archive")
	if archive == "" {
		return pkgRef{}, errors.New("no x-debuginfod-archive header")
	}
	return parseArchive(archive)
}

// indexFor returns the index of a package's source tree, unpacking it first if
// needed. The index is cached per package: an unpacked tree is immutable, so it
// cannot go stale, and rebuilding it per request would walk twenty thousand
// files each time.
//
// NOT YET EVICTED - see the note in the README. glibc alone is 294 MB unpacked.
func (s *srv) indexFor(ctx context.Context, pkg pkgRef) (*srcindex.Index, error) {
	key := pkg.Component + "/" + pkg.Source + "_" + pkg.Version

	s.mu.Lock()
	if ix, ok := s.indexes[key]; ok {
		s.mu.Unlock()
		return ix, nil
	}
	s.mu.Unlock()
	if err, ok := s.rememberedFailure(key); ok {
		return nil, err
	}

	res, err, _ := s.group.Do(key, func() (any, error) {
		root, err := s.unpack(ctx, pkg)
		if err != nil {
			s.rememberFailure(ctx, key, err)
			return nil, err
		}
		ix, err := srcindex.New(root)
		if err != nil {
			return nil, err
		}
		if len(ix.Skipped) > 0 {
			log.WithField("package", key).WithField("trees", ix.Skipped).
				Warn("some source trees could not be read")
		}
		s.mu.Lock()
		s.indexes[key] = ix
		s.mu.Unlock()
		return ix, nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*srcindex.Index), nil
}

// errSourcesMissing means the unpacker found the package in neither the pool
// nor its fallback.
var errSourcesMissing = errors.New("source package not in the archive or its fallback")

// errUpstream marks a failure behind this service: the unpacker unreachable,
// or the unpacker reporting that the pool or its fallback failed (its 502).
var errUpstream = errors.New("upstream failed")

// prepareStatus maps a failure to prepare a package to a status. 502 when it
// happened behind us; 500 when it was ours - the unpacker's own 500
// (dpkg-source, its disk), or indexing the tree it produced. Telling the two
// apart is what points whoever reads the logs at Debian or at this host.
func prepareStatus(err error) int {
	if errors.Is(err, errUpstream) {
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

func (s *srv) rememberedFailure(key string) (error, bool) {
	if s.missing != nil {
		if err, ok := s.missing.Get(key); ok {
			return err, true
		}
	}
	if s.failed != nil {
		if err, ok := s.failed.Get(key); ok {
			return err, true
		}
	}
	return nil, false
}

// rememberFailure records why a package could not be prepared - unless the
// request was cancelled. A client hanging up says nothing about the package,
// and since singleflight makes every waiter share the leader's fate, caching
// that would hand one client's disconnect to everyone for a minute.
func (s *srv) rememberFailure(ctx context.Context, key string, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, errSourcesMissing) {
		if s.missing != nil {
			s.missing.Add(key, err)
		}
		return
	}
	if s.failed != nil {
		s.failed.Add(key, err)
	}
}

func (s *srv) unpack(ctx context.Context, pkg pkgRef) (string, error) {
	body, err := json.Marshal(map[string]string{
		"source": pkg.Source, "version": pkg.Version, "component": pkg.Component,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.unpacker+"/unpack", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: unpacker unreachable: %w", errUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		switch resp.StatusCode {
		case http.StatusNotFound:
			return "", fmt.Errorf("%w: %s", errSourcesMissing, bytes.TrimSpace(msg))
		case http.StatusBadGateway:
			return "", fmt.Errorf("%w: unpacker: %s: %s", errUpstream, resp.Status, bytes.TrimSpace(msg))
		}
		return "", fmt.Errorf("unpacker: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}

	var out struct {
		Path   string `json:"path"`
		Files  int    `json:"files"`
		Cached bool   `json:"cached"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Path == "" {
		return "", errors.New("unpacker returned no path")
	}
	log.WithField("package", pkg.Source+"_"+pkg.Version).
		WithField("files", out.Files).WithField("cached", out.Cached).Debug("sources ready")
	return out.Path, nil
}
