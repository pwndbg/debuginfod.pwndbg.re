// Command deb-debuginfod serves /buildid/<id>/source/<path> for Debian build
// IDs, and nothing else.
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
	"golang.org/x/sync/singleflight"
)

const (
	// debianDebuginfod is asked for headers only; it is the authority on which
	// .deb a build ID belongs to.
	debianDebuginfod = "https://debuginfod.debian.net"

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
)

type srv struct {
	// debian is the authority on which .deb a build ID belongs to. A field
	// rather than the constant so a test can stand in for it and count how
	// often it is actually asked.
	debian   string
	unpacker string
	hc       *http.Client

	mu      sync.Mutex
	indexes map[string]*srcindex.Index
	// lastUsed drives eviction: when each package was last actually served.
	lastUsed map[string]time.Time
	group    singleflight.Group

	resolved *expirable.LRU[string, pkgRef]
	// resolveGroup coalesces concurrent lookups of the same cold build ID. A
	// debugger opens several files at once, and without this each one would
	// make its own request to Debian for the identical answer.
	resolveGroup singleflight.Group
}

func main() {
	addr := envOr("LISTEN_ADDR", "127.0.0.1:8036")
	s := &srv{
		debian:   envOr("DEBIAN_DEBUGINFOD", debianDebuginfod),
		unpacker: envOr("UNPACKER_URL", "http://127.0.0.1:8035"),
		indexes:  map[string]*srcindex.Index{},
		lastUsed: map[string]time.Time{},
		resolved: expirable.NewLRU[string, pkgRef](resolveCacheSize, nil, resolveCacheTTL),
		// No overall timeout: unpacking a source package is minutes on a cold
		// package. Per-request contexts bound the parts that should be quick.
		hc: &http.Client{},
	}

	router := httprouter.New()
	router.GET("/buildid/:buildid/source/*path", s.handleSource)
	// Answered explicitly rather than left to 404 so a misconfigured caller
	// learns what this service is, instead of concluding the build ID is
	// unknown. cmd/proxy should route only source here.
	router.GET("/buildid/:buildid/debuginfo", notImplemented)
	router.GET("/buildid/:buildid/executable", notImplemented)
	router.GET("/status", func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}
	budget := envInt("CACHE_MAX_BYTES", 20<<30)
	interval := envDuration("EVICT_INTERVAL", 10*time.Minute)
	go s.EvictLoop(context.Background(), interval, budget)

	log.WithField("addr", addr).WithField("unpacker", s.unpacker).
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

func notImplemented(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	http.Error(w, "this backend serves /source/* only; debuginfo and executable come from debuginfod.debian.net",
		http.StatusNotImplemented)
}

func (s *srv) handleSource(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	buildID := ps.ByName("buildid")
	reqPath := ps.ByName("path")
	started := time.Now()
	logger := log.WithField("buildid", buildID).WithField("path", reqPath)

	pkg, err := s.resolve(r.Context(), buildID)
	if err != nil {
		logger.WithError(err).Debug("build ID does not resolve to a Debian package")
		http.Error(w, "unknown build ID", http.StatusNotFound)
		return
	}
	logger = logger.WithField("package", pkg.Source+"_"+pkg.Version)

	key := pkg.Component + "/" + pkg.Source + "_" + pkg.Version
	s.touch(key)

	ix, err := s.indexFor(r.Context(), pkg)
	if err != nil {
		logger.WithError(err).Warn("preparing sources failed")
		http.Error(w, "sources unavailable", http.StatusBadGateway)
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
		logger.WithError(err).Warn("opening a file the index just offered")
		http.Error(w, "sources unavailable", http.StatusBadGateway)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "sources unavailable", http.StatusBadGateway)
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

// resolve asks Debian which package a build ID belongs to.
//
// It reads the headers and closes the body immediately: the debuginfo payload
// is megabytes and none of it is wanted. HEAD is not used because debuginfod
// servers are not required to implement it, and a GET whose body is dropped is
// the portable way to ask the same question.
func (s *srv) resolve(ctx context.Context, buildID string) (pkgRef, error) {
	if pkg, ok := s.resolved.Get(buildID); ok {
		return pkg, nil
	}
	res, err, _ := s.resolveGroup.Do(buildID, func() (any, error) {
		pkg, err := s.askDebian(ctx, buildID)
		if err != nil {
			// Deliberately not cached. A negative answer here is usually
			// "Debian was slow" rather than "this build ID does not exist",
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

func (s *srv) askDebian(ctx context.Context, buildID string) (pkgRef, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	url := s.debian + "/buildid/" + buildID + "/debuginfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return pkgRef{}, err
	}
	req.Header.Set("user-agent", "pwndbg-debuginfod-deb")

	resp, err := s.hc.Do(req)
	if err != nil {
		return pkgRef{}, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return pkgRef{}, fmt.Errorf("debian answered %s", resp.Status)
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

	res, err, _ := s.group.Do(key, func() (any, error) {
		root, err := s.unpack(ctx, pkg)
		if err != nil {
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
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
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
