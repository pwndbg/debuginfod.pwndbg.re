package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/pwndbg/debuginfod.pwndbg.re/pkgarchive"
)

// entry is one indexed_buildids row: where a build id can be fetched from.
type entry struct {
	Kind    string // "executable" or "debuginfo", see cmd/indexer/archive.go
	URL     string
	Format  string
	Archive string // x-debuginfod-archive
	Member  string
	Size    uint64
}

// index rather than a ClickHouse connection, so the handler tests need no
// database - the same reason cmd/proxy has stateStore.
type index interface {
	Lookup(ctx context.Context, source, buildID string) ([]entry, error)
}

// knownSources are the prefixes cmd/indexer writes. Anything else is a 404
// without a query, so a mistyped upstream URL in cmd/proxy shows up as "nothing
// here" in its resolve logs instead of as load on ClickHouse.
var knownSources = map[string]bool{"pbs": true, "ubuntu": true}

const (
	lookupCacheSize = 10000
	// lookupCacheTTL bounds how long a miss is believed. cmd/proxy probes every
	// upstream for every cold build id, so most lookups here are misses, and
	// the indexer adds rows only every few hours - but not much longer than
	// that, or a fresh SRU would wait a day to become servable.
	lookupCacheTTL = 10 * time.Minute
)

type srv struct {
	idx    index
	hc     *http.Client
	lookup *expirable.LRU[string, []entry]
}

func (s *srv) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{source}/buildid/{id}/{endpoint}", s.handleArtifact)
	// Answered rather than left to 404, so cmd/proxy records "this host has no
	// sources" - a 501 - and not "this build id does not exist". Sources for
	// the Ubuntu rows come from cmd/deb-debuginfod via SourceVia.
	mux.HandleFunc("GET /{source}/buildid/{id}/source/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "this backend serves debuginfo and executable only", http.StatusNotImplemented)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {})
	return mux
}

// validBuildID mirrors cmd/proxy's check: the id goes into a query, and
// anything the proxy would not forward cannot be in the index either.
func validBuildID(id string) bool {
	if len(id) != 40 && len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *srv) find(ctx context.Context, source, buildID string) ([]entry, error) {
	key := source + "\x00" + buildID
	if es, ok := s.lookup.Get(key); ok {
		return es, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	es, err := s.idx.Lookup(ctx, source, buildID)
	if err != nil {
		// Not cached: a ClickHouse blip must not become ten minutes of 404s.
		return nil, err
	}
	s.lookup.Add(key, es)
	return es, nil
}

var (
	// errGone: this URL no longer has the archive. The next candidate may.
	errGone = errors.New("archive is gone")
	// errMemberMissing: the archive is there but the member is not - the index
	// and the archive disagree, which is our bug, not the mirror's.
	errMemberMissing = errors.New("member not in archive")
)

func (s *srv) handleArtifact(w http.ResponseWriter, r *http.Request) {
	source, id, endpoint := r.PathValue("source"), r.PathValue("id"), r.PathValue("endpoint")
	if !knownSources[source] || !validBuildID(id) {
		http.NotFound(w, r)
		return
	}
	if endpoint != "debuginfo" && endpoint != "executable" {
		http.NotFound(w, r)
		return
	}

	entries, err := s.find(r.Context(), source, id)
	if err != nil {
		log.WithError(err).WithField("build_id", id).Error("index lookup failed")
		http.Error(w, "index lookup failed", http.StatusInternalServerError)
		return
	}
	// A separate .debug has its code sections stripped to NOBITS. Serving it as
	// an executable would hand a debugger a file that looks right and
	// disassembles as zeros.
	var usable []entry
	for _, e := range entries {
		if endpoint == "debuginfo" || e.Kind == "executable" {
			usable = append(usable, e)
		}
	}
	if len(usable) == 0 {
		http.NotFound(w, r)
		return
	}
	// The pool is a mirror network; Launchpad's librarian is one service. Ask
	// the pool first and fall back when it has dropped the version.
	sort.SliceStable(usable, func(i, j int) bool {
		return !strings.Contains(usable[i].URL, "launchpad") && strings.Contains(usable[j].URL, "launchpad")
	})

	first := usable[0]
	h := w.Header()
	h.Set("X-Debuginfod-Archive", first.Archive)
	h.Set("X-Debuginfod-File", "/"+first.Member)
	h.Set("X-Debuginfod-Size", strconv.FormatUint(first.Size, 10))

	// The answer is known now; the bytes are a download away. cmd/proxy probes
	// with a 5 s header budget, and the member of a python asset can sit
	// 100 MB into it - so it gets the answer as Early Hints, the same protocol
	// cmd/nix-debuginfod speaks. A 103 commits nothing: a mirror failure below
	// can still be a 502 rather than a 200 with a broken body.
	h.Set("X-Status", "200")
	w.WriteHeader(http.StatusEarlyHints)
	h.Del("X-Status")

	err = errGone
	for _, e := range usable {
		if err = s.serve(w, r, e); !errors.Is(err, errGone) {
			break
		}
		log.WithField("url", e.URL).Info("archive gone, trying the next copy")
	}
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		// The client left - a probe that had its answer, usually.
	case errors.Is(err, errGone):
		fail(w, "every copy of the archive is gone", http.StatusNotFound)
	case errors.Is(err, errMemberMissing):
		log.WithError(err).WithField("build_id", id).Error("index disagrees with archive")
		fail(w, err.Error(), http.StatusInternalServerError)
	default:
		log.WithError(err).WithField("build_id", id).Warn("fetching archive failed")
		fail(w, err.Error(), http.StatusBadGateway)
	}
}

// fail answers with an error after the Early Hints. The x-debuginfod-* headers
// set for the hint are still in the map and would otherwise ride along on the
// error, describing a file this response does not carry.
func fail(w http.ResponseWriter, msg string, code int) {
	for _, k := range []string{"X-Debuginfod-Archive", "X-Debuginfod-File", "X-Debuginfod-Size"} {
		w.Header().Del(k)
	}
	http.Error(w, msg, code)
}

// serve fetches one archive and streams one member out of it. It returns an
// error only before the response is committed; after that, a failure breaks
// the connection, because a 200 with a short body that ends cleanly is the one
// outcome cmd/proxy cannot tell from success.
func (s *srv) serve(w http.ResponseWriter, r *http.Request, e entry) error {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, e.URL, nil)
	if err != nil {
		return err
	}
	// See ScanArchive in cmd/indexer: the format column describes the file as
	// stored, so Go's transport must not decode a Content-Encoding: gzip away.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errGone
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("GET %s: %s", e.URL, resp.Status)
	}

	tr, closeFn, err := pkgarchive.Tar(e.Format, resp.Body)
	if err != nil {
		return fmt.Errorf("opening %s: %w", e.URL, err)
	}
	defer closeFn()

	var hdr *tar.Header
	for {
		hdr, err = tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: %s in %s", errMemberMissing, e.Member, e.URL)
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", e.URL, err)
		}
		if hdr.Typeflag == tar.TypeReg && pkgarchive.Member(hdr.Name) == e.Member {
			break
		}
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(hdr.Size, 10))
	h.Set("X-Debuginfod-Archive", e.Archive)
	h.Set("X-Debuginfod-File", "/"+e.Member)
	h.Set("X-Debuginfod-Size", strconv.FormatInt(hdr.Size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, tr); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

// chIndex reads indexed_buildids.
type chIndex struct {
	conn driver.Conn
}

func (c *chIndex) Lookup(ctx context.Context, source, buildID string) ([]entry, error) {
	// No FINAL: duplicates from a re-index name the same url and member, and
	// are folded below. Reading the primary key prefix is a point lookup.
	rows, err := c.conn.Query(ctx, `
SELECT kind, url, format, archive, member, member_size
FROM indexed_buildids
WHERE buildid = ? AND source = ?`, buildID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []entry
	seen := map[string]bool{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Kind, &e.URL, &e.Format, &e.Archive, &e.Member, &e.Size); err != nil {
			return nil, err
		}
		if k := e.URL + "\x00" + e.Member; !seen[k] {
			seen[k] = true
			out = append(out, e)
		}
	}
	return out, rows.Err()
}
