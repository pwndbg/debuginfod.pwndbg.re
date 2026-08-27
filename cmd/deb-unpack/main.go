// Command deb-unpack turns a Debian source package into an unpacked, patched
// source tree on a shared volume.
//
// It exists as its own service for one reason: it needs dpkg-dev, and
// dpkg-source is the only thing that knows how to turn a .dsc into the sources
// the binary was actually built from. Debian's 3.0 (quilt) format keeps the
// pristine upstream tarball and a series of patches side by side, and the
// difference is not cosmetic - for glibc 2.41-12+deb13u3 the series changes 666
// source files, elf/rtld.c among them by 97 lines. Serving pristine there would
// show a debugger the wrong lines, silently.
//
// Reimplementing that in Go was considered and rejected. dpkg-source handles
// four source formats, multiple orig tarballs, binary patches and quilt
// semantics; a partial reimplementation would fail on exactly the packages
// nobody tests. Instead this runs in debian:13-slim, where dpkg-dev costs 20
// packages and ~94 MB, and cmd/deb-debuginfod stays on a minimal image and
// talks to it over HTTP.
//
// Deliberately NOT reachable from anywhere but the serving container: it takes
// a package name and shells out, so it is an internal component, not an API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

// doneMarker is written into a tree only after dpkg-source has succeeded and
// the directory has been renamed into place. Its absence means "not usable",
// which is what makes a killed process safe: a half-unpacked tree is either
// still under its temporary name or missing this file, never both complete and
// wrong.
const doneMarker = ".unpack-complete"

// staleUnpackAge is how old a staging directory must be before it is reclaimed.
// Generous on purpose: a large package can take minutes, and deleting a live
// unpack fails a request to free space that was about to be used anyway.
const staleUnpackAge = 2 * time.Hour

// Debian package names and versions become both filesystem paths and command
// arguments here, and they arrive from a header on a third-party server. These
// patterns are what policy actually allows, and everything else is refused
// rather than sanitised - a name that needs cleaning is a name we do not
// understand.
var (
	rePackage = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,63}$`)
	reVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+~-]{0,63}$`)
	reComp    = regexp.MustCompile(`^(main|contrib|non-free|non-free-firmware)$`)
)

type unpackRequest struct {
	Source    string `json:"source"`
	Version   string `json:"version"`
	Component string `json:"component"`
}

type unpackResponse struct {
	Path   string `json:"path"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
	Took   string `json:"took"`
	Cached bool   `json:"cached"`
}

type server struct {
	root   string
	mirror string
	hc     *http.Client
	group  singleflight.Group
}

func main() {
	var (
		addr   = envOr("LISTEN_ADDR", "127.0.0.1:8035")
		root   = envOr("UNPACK_PATH", "/var/lib/deb-src")
		mirror = envOr("DEBIAN_MIRROR", "https://deb.debian.org/debian")
	)
	if _, err := exec.LookPath("dpkg-source"); err != nil {
		// Fatal rather than degraded: without dpkg-source this service can do
		// nothing at all, and finding that out per request would turn a
		// packaging mistake into a slow trickle of 500s.
		log.WithError(err).Fatal("dpkg-source not found - this image needs dpkg-dev")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		log.WithError(err).Fatal("creating unpack root")
	}

	s := &server{root: root, mirror: mirror, hc: &http.Client{Timeout: 10 * time.Minute}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /unpack", s.handleUnpack)
	mux.HandleFunc("GET /trees", s.handleTrees)
	mux.HandleFunc("DELETE /tree/{component}/{name}", s.handleDelete)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// No write timeout: unpacking a large source package is minutes of work
		// and the caller is a service, not a browser. Cancellation comes from
		// the request context instead.
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Abandoned staging directories are reclaimed on startup and then hourly.
	// A killed process leaves them behind, and nothing else ever will.
	s.sweepStale(staleUnpackAge)
	go func() {
		for range time.Tick(time.Hour) {
			s.sweepStale(staleUnpackAge)
		}
	}()

	log.WithField("addr", addr).WithField("root", root).Info("deb-unpack starting")
	if err := srv.ListenAndServe(); err != nil {
		log.WithError(err).Fatal("server failed")
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *server) handleUnpack(w http.ResponseWriter, r *http.Request) {
	var req unpackRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if req.Component == "" {
		req.Component = "main"
	}
	if !rePackage.MatchString(req.Source) || !reVersion.MatchString(req.Version) || !reComp.MatchString(req.Component) {
		http.Error(w, "invalid source, version or component", http.StatusBadRequest)
		return
	}

	started := time.Now()
	key := req.Component + "/" + req.Source + "_" + req.Version
	// Two requests for the same package must not unpack it twice: the second
	// would race the first's rename and, worse, both would pay the download.
	res, err, shared := s.group.Do(key, func() (any, error) {
		return s.unpack(r.Context(), req)
	})
	if err != nil {
		log.WithError(err).WithField("package", key).Warn("unpack failed")
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	out := res.(unpackResponse)
	out.Took = time.Since(started).Round(time.Millisecond).String()
	if shared {
		log.WithField("package", key).Debug("coalesced onto an in-flight unpack")
	}
	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// unpack produces the tree, or returns the existing one.
func (s *server) unpack(ctx context.Context, req unpackRequest) (unpackResponse, error) {
	final := filepath.Join(s.root, req.Component, req.Source+"_"+req.Version)
	if _, err := os.Stat(filepath.Join(final, doneMarker)); err == nil {
		files, bytes := measure(final)
		return unpackResponse{Path: final, Files: files, Bytes: bytes, Cached: true}, nil
	}

	// Everything happens under a temporary directory that is renamed into place
	// at the very end, so a crash leaves rubbish rather than a tree that looks
	// finished. The staging area holds the .dsc and its tarballs: dpkg-source
	// requires them side by side.
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return unpackResponse{}, err
	}
	work, err := os.MkdirTemp(filepath.Dir(final), ".tmp-"+req.Source+"-")
	if err != nil {
		return unpackResponse{}, err
	}
	defer os.RemoveAll(work)

	base := fmt.Sprintf("%s/pool/%s/%s/%s", s.mirror, req.Component, poolDir(req.Source), req.Source)
	dsc := req.Source + "_" + req.Version + ".dsc"
	if err := s.download(ctx, base+"/"+dsc, filepath.Join(work, dsc)); err != nil {
		return unpackResponse{}, fmt.Errorf("fetching %s: %w", dsc, err)
	}
	files, err := filesFromDsc(filepath.Join(work, dsc))
	if err != nil {
		return unpackResponse{}, err
	}
	for _, f := range files {
		if err := s.download(ctx, base+"/"+f, filepath.Join(work, f)); err != nil {
			return unpackResponse{}, fmt.Errorf("fetching %s: %w", f, err)
		}
	}

	out := filepath.Join(work, "tree")
	// --no-check skips the OpenPGP signature check. That is a deliberate
	// choice, not an oversight: verifying needs the Debian keyring in this
	// image, and the bytes already came over TLS from the same archive that
	// signed them. Revisit if this ever fetches from an untrusted mirror.
	cmd := exec.CommandContext(ctx, "dpkg-source", "--no-check", "-x", filepath.Join(work, dsc), out)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return unpackResponse{}, fmt.Errorf("dpkg-source: %w: %s", err, lastLines(string(combined), 3))
	}
	if err := os.WriteFile(filepath.Join(out, doneMarker), nil, 0o644); err != nil {
		return unpackResponse{}, err
	}
	if err := os.Rename(out, final); err != nil {
		// Losing the rename to a concurrent unpacker is fine - the tree is
		// there either way, and both would have produced the same bytes.
		if _, statErr := os.Stat(filepath.Join(final, doneMarker)); statErr != nil {
			return unpackResponse{}, err
		}
	}

	n, size := measure(final)
	log.WithField("package", req.Source+"_"+req.Version).
		WithField("files", n).WithField("bytes", size).Info("unpacked")
	return unpackResponse{Path: final, Files: n, Bytes: size}, nil
}

func (s *server) download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("user-agent", "pwndbg-debuginfod-deb-unpack")
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// filesFromDsc reads the Files: stanza. Only the base names are taken, and any
// that is not a plain name is refused: these strings come from a downloaded
// file and are about to be joined onto a path.
func filesFromDsc(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	inFiles := false
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, " ") {
			inFiles = strings.HasPrefix(line, "Files:")
			continue
		}
		if !inFiles {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		name := fields[2]
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
			return nil, fmt.Errorf("refusing suspicious file name in .dsc: %q", name)
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, errors.New(".dsc lists no files")
	}
	return out, nil
}

// poolDir is the archive's shard directory: the first letter, except for lib*
// packages which get their own level. This mirrors the layout, it does not
// choose it.
func poolDir(source string) string {
	if strings.HasPrefix(source, "lib") && len(source) > 3 {
		return source[:4]
	}
	return source[:1]
}

func measure(root string) (int, int64) {
	var n int
	var size int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n++
		if info, err := d.Info(); err == nil {
			size += info.Size()
		}
		return nil
	})
	return n, size
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
