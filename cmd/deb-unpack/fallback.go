package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// errNotFound is a 404 from wherever a file was being fetched - the one answer
// that justifies asking the fallback. Any other failure is reported as is: a
// 503 from the mirror says nothing about whether the file exists, and moving
// that load onto an archive that is not a mirror would hide the outage.
var errNotFound = errors.New("404 Not Found")

// sourceArchive is a permanent archive of source packages, asked only for what
// the mirror's pool no longer has.
//
// Both pools drop superseded versions. Debian keeps a suite's current version
// and little else (glibc 2.41-1 to 2.41-10 are gone from deb.debian.org), and
// archive.ubuntu.com drops every superseded stable update (coreutils
// 8.32-4.1ubuntu1.2). We still serve the debuginfo of those builds from cache,
// and the proxy passes on the archive it recorded for them, so the request does
// reach this service - and without a fallback the pool's 404 is the answer.
type sourceArchive interface {
	// fetch downloads the .dsc of src_ver and every file it lists into work.
	// errNotFound means the archive does not have that version either.
	fetch(ctx context.Context, s *server, src, ver, work string) error
}

const (
	defaultLaunchpad = "https://launchpad.net/ubuntu/+archive/primary/+sourcefiles"
	defaultSnapshot  = "https://snapshot.debian.org"
)

// fallbackFromEnv maps FALLBACK to an archive. An unknown name is an error
// rather than "no fallback": a typo there would otherwise disable it silently,
// and nothing would notice until a dropped version 404'd in production.
func fallbackFromEnv(name, url string) (sourceArchive, error) {
	switch name {
	case "":
		return nil, nil
	case "launchpad":
		if url == "" {
			url = defaultLaunchpad
		}
		return launchpad{base: strings.TrimRight(url, "/")}, nil
	case "snapshot":
		if url == "" {
			url = defaultSnapshot
		}
		return snapshot{base: strings.TrimRight(url, "/")}, nil
	}
	return nil, fmt.Errorf("unknown fallback %q (want launchpad, snapshot or empty)", name)
}

func fallbackName(a sourceArchive) string {
	switch a.(type) {
	case launchpad:
		return "launchpad"
	case snapshot:
		return "snapshot"
	}
	return "none"
}

// fetchSources downloads the .dsc and every file it lists into work and returns
// the .dsc's name.
//
// The pool first; the fallback only when the pool has no .dsc for this version.
// Every file then comes from the fallback, even ones the pool still has - the
// orig tarball usually outlives the Debian revision that dropped out - so a tree
// is never assembled from two places that could disagree about what a name means.
func (s *server) fetchSources(ctx context.Context, req unpackRequest, work string) (string, error) {
	dsc := req.Source + "_" + req.Version + ".dsc"
	base := fmt.Sprintf("%s/pool/%s/%s/%s", s.mirror, req.Component, poolDir(req.Source), req.Source)

	err := s.download(ctx, base+"/"+dsc, filepath.Join(work, dsc))
	if err == nil {
		files, err := filesFromDsc(filepath.Join(work, dsc))
		if err != nil {
			return "", err
		}
		for _, f := range files {
			if err := s.download(ctx, base+"/"+f, filepath.Join(work, f)); err != nil {
				return "", fmt.Errorf("fetching %s: %w", f, err)
			}
		}
		return dsc, nil
	}
	if !errors.Is(err, errNotFound) || s.fallback == nil {
		return "", fmt.Errorf("fetching %s: %w", dsc, err)
	}

	log.WithField("package", req.Source+"_"+req.Version).WithField("fallback", fallbackName(s.fallback)).
		Info("not in the mirror's pool; asking the fallback")
	if err := s.fallback.fetch(ctx, s, req.Source, req.Version, work); err != nil {
		return "", fmt.Errorf("%s: %w", fallbackName(s.fallback), err)
	}
	return dsc, nil
}

// launchpad serves every source upload Ubuntu ever published, addressed by
// name and version - <base>/<src>/<ver>/<file> - and redirects to its librarian
// for the bytes. Verified back to 14.04 (coreutils 8.21-1ubuntu5).
//
// snapshot.ubuntu.com exists too and is not used: it is addressed by date, so it
// needs to know when a version was in the archive, and it starts in 2023.
type launchpad struct{ base string }

func (l launchpad) fetch(ctx context.Context, s *server, src, ver, work string) error {
	base := l.base + "/" + src + "/" + ver
	dsc := src + "_" + ver + ".dsc"
	if err := s.download(ctx, base+"/"+dsc, filepath.Join(work, dsc)); err != nil {
		return fmt.Errorf("fetching %s: %w", dsc, err)
	}
	files, err := filesFromDsc(filepath.Join(work, dsc))
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := s.download(ctx, base+"/"+f, filepath.Join(work, f)); err != nil {
			return fmt.Errorf("fetching %s: %w", f, err)
		}
	}
	return nil
}

// snapshot is snapshot.debian.org, which keeps every version Debian ever
// published (664 of glibc). Files are addressed by content, not by name: one
// request lists the package's files with their sha1, and each is then fetched
// from /file/<sha1>.
//
// Because the address IS the hash, every download is checked against it. That
// costs nothing and matters here, since dpkg-source runs with --no-check.
//
// The service asks to be used politely. This is one listing plus a few files per
// package, once - the tree is cached after that - so it is a trickle.
type snapshot struct{ base string }

func (sn snapshot) fetch(ctx context.Context, s *server, src, ver, work string) error {
	byName, err := sn.list(ctx, s, src, ver)
	if err != nil {
		return err
	}
	dsc := src + "_" + ver + ".dsc"
	if err := sn.get(ctx, s, byName, dsc, work); err != nil {
		return err
	}
	files, err := filesFromDsc(filepath.Join(work, dsc))
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := sn.get(ctx, s, byName, f, work); err != nil {
			return err
		}
	}
	return nil
}

// list returns name -> sha1 for one source package version.
func (sn snapshot) list(ctx context.Context, s *server, src, ver string) (map[string]string, error) {
	url := fmt.Sprintf("%s/mr/package/%s/%s/srcfiles?fileinfo=1", sn.base, src, ver)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing %s_%s: %s", src, ver, resp.Status)
	}
	var body struct {
		FileInfo map[string][]struct {
			Name string `json:"name"`
		} `json:"fileinfo"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("listing %s_%s: %w", src, ver, err)
	}
	byName := map[string]string{}
	for hash, infos := range body.FileInfo {
		for _, i := range infos {
			byName[i.Name] = hash
		}
	}
	return byName, nil
}

// get downloads one file by name and checks it against its sha1.
func (sn snapshot) get(ctx context.Context, s *server, byName map[string]string, name, work string) error {
	hash, ok := byName[name]
	if !ok {
		return fmt.Errorf("fetching %s: not in the snapshot's file list: %w", name, errNotFound)
	}
	dst := filepath.Join(work, name)
	if err := s.download(ctx, sn.base+"/file/"+hash, dst); err != nil {
		return fmt.Errorf("fetching %s: %w", name, err)
	}
	got, err := sha1File(dst)
	if err != nil {
		return err
	}
	if got != hash {
		return fmt.Errorf("fetching %s: sha1 %s, snapshot lists %s", name, got, hash)
	}
	return nil
}

func sha1File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
