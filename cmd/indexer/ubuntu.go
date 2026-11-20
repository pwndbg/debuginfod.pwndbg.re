package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Ubuntu ships the debug info of a few packages as an ordinary binary package
// in the archive rather than as a .ddeb - glibc above all: libc.so.6 and
// ld-linux are in libc6-dbg, and libc6-dbgsym exists only for 16.04.
// debuginfod.ubuntu.com indexes ddebs.ubuntu.com and nothing else, so it
// answers 404 for the libc of every Ubuntu release since 18.04. Measured: of
// the build ids in libc6-dbg 2.43-2ubuntu2, 2.39-0ubuntu8.12 and
// 2.35-0ubuntu3.18 (28, 273, 292) it knew none.
//
// The fix is to index those packages ourselves. A package is named as
// component/source/binary because all three are needed: the pool directory is
// the SOURCE (glibc), the file is the BINARY (libc6-dbg), and the header passed
// on to cmd/deb-debuginfod must carry the component so it can find the sources.
type ubuntuPackage struct {
	Component, Source, Binary string
}

func parseUbuntuPackages(spec string) ([]ubuntuPackage, error) {
	var out []ubuntuPackage
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		parts := strings.Split(p, "/")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return nil, fmt.Errorf("package %q: want component/source/binary, e.g. main/glibc/libc6-dbg", p)
		}
		out = append(out, ubuntuPackage{Component: parts[0], Source: parts[1], Binary: parts[2]})
	}
	return out, nil
}

// poolShard is the pool's first-level directory: the first letter, except that
// lib* sources get four characters - pool/main/libx/libxml2.
func poolShard(source string) string {
	if strings.HasPrefix(source, "lib") && len(source) > 3 {
		return source[:4]
	}
	return source[:1]
}

// ubuntuArchive builds the index entry for one .deb. The header is pool-shaped
// whichever URL the file is fetched from, because what cmd/deb-debuginfod
// reads out of it - component, source, version - does not depend on that.
func ubuntuArchive(p ubuntuPackage, component, version, arch, fileURL string) Archive {
	name := fmt.Sprintf("%s_%s_%s.deb", p.Binary, version, arch)
	return Archive{
		Source: sourceUbuntu, URL: fileURL, Name: name, Format: "deb",
		Package: p.Binary, Version: version, Arch: arch,
		Header: fmt.Sprintf("/ubuntu/pool/%s/%s/%s/%s", component, poolShard(p.Source), p.Source, name),
		Kind:   kindDebuginfo,
	}
}

var hrefRE = regexp.MustCompile(`href="([^"?/]+\.deb)"`)

// listPool reads the pool directory listing. It is the cheap, current half:
// one request per package, and it holds the release version and the newest
// update of every supported release - which is what most machines run.
func listPool(ctx context.Context, hc *http.Client, mirror string, p ubuntuPackage, arches map[string]bool) ([]Archive, error) {
	dir := fmt.Sprintf("%s/pool/%s/%s/%s/", strings.TrimRight(mirror, "/"), p.Component, poolShard(p.Source), p.Source)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dir, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", dir, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}

	var out []Archive
	for _, m := range hrefRE.FindAllStringSubmatch(string(body), -1) {
		name, err := url.PathUnescape(m[1])
		if err != nil {
			continue
		}
		version, arch, ok := splitDebName(name, p.Binary)
		if !ok || !arches[arch] {
			continue
		}
		a := ubuntuArchive(p, p.Component, version, arch, dir+url.PathEscape(name))
		a.Fallback = launchpadFiles + url.PathEscape(name)
		out = append(out, a)
	}
	return out, nil
}

// splitDebName reads <binary>_<version>_<arch>.deb, for one binary only - the
// glibc directory also holds libc6, libc6-dev, locales and twenty others.
func splitDebName(name, binary string) (version, arch string, ok bool) {
	rest, found := strings.CutPrefix(name, binary+"_")
	if !found {
		return "", "", false
	}
	rest, found = strings.CutSuffix(rest, ".deb")
	if !found {
		return "", "", false
	}
	i := strings.LastIndex(rest, "_")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// launchpadAPIURL is a variable only so the tests can point it at a fake.
var launchpadAPIURL = "https://api.launchpad.net/1.0/ubuntu/+archive/primary"

// launchpadFiles serves every binary the primary archive ever published, after
// the pool has dropped it. +files redirects to the librarian.
const launchpadFiles = "https://launchpad.net/ubuntu/+archive/primary/+files/"

type lpPage struct {
	Entries []struct {
		Version   string `json:"binary_package_version"`
		ArchLink  string `json:"distro_arch_series_link"`
		Component string `json:"component_name"`
	} `json:"entries"`
	Next string `json:"next_collection_link"`
}

// listLaunchpad is the history half. The pool keeps only each release's
// original version and its newest update; every SRU in between is gone from it
// while still installed on machines that have not upgraded, and in container
// images that pinned it. Launchpad lists them all - for libc6-dbg some 6,000
// publishing records, which collapse to a few hundred (version, arch) pairs.
//
// skip holds the pool's own files: those are fetched from the pool, which is a
// mirror network rather than one service.
//
// window limits it to the series worth having; see seriesWindow.
func listLaunchpad(ctx context.Context, hc *http.Client, p ubuntuPackage, arches map[string]bool, skip map[string]bool, window seriesWindow) ([]Archive, error) {
	q := url.Values{
		"ws.op":       {"getPublishedBinaries"},
		"binary_name": {p.Binary},
		"exact_match": {"true"},
		"ws.size":     {"300"},
	}
	if !window.from.IsZero() {
		// Server-side, so the pages for twenty years of older series are
		// never sent. The series filter below is still needed: this cuts by
		// publication date, and a newer series' window opens while the
		// previous series is still taking updates.
		q.Set("created_since_date", window.from.UTC().Format("2006-01-02"))
	}
	next := launchpadAPIURL + "?" + q.Encode()
	seen := map[string]bool{}
	var out []Archive
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		var page lpPage
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s: %s", next, resp.Status)
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decoding %s: %w", next, err)
		}
		for _, e := range page.Entries {
			parts := strings.Split(e.ArchLink, "/")
			if len(parts) < 2 {
				continue
			}
			series, arch := parts[len(parts)-2], parts[len(parts)-1]
			if e.Version == "" || !arches[arch] || !window.allows(series) {
				continue
			}
			name := fmt.Sprintf("%s_%s_%s.deb", p.Binary, e.Version, arch)
			if seen[name] || skip[name] {
				continue
			}
			seen[name] = true
			component := e.Component
			if component == "" {
				component = p.Component
			}
			out = append(out, ubuntuArchive(p, component, e.Version, arch, launchpadFiles+url.PathEscape(name)))
		}
		next = page.Next
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// collectUbuntu lists the pool for every package and, with history, Launchpad.
func collectUbuntu(ctx context.Context, hc *http.Client, opt options) ([]Archive, error) {
	pkgs, err := parseUbuntuPackages(opt.ubuntuPackages)
	if err != nil {
		return nil, err
	}
	arches := map[string]bool{}
	for _, a := range strings.Split(opt.ubuntuArches, ",") {
		if a = strings.TrimSpace(a); a != "" {
			arches[a] = true
		}
	}

	var window seriesWindow
	if opt.ubuntuHistory {
		if window, err = ubuntuSeriesSince(ctx, hc, opt.ubuntuSince); err != nil {
			return nil, err
		}
	}

	var out []Archive
	for _, p := range pkgs {
		pool, err := listPool(ctx, hc, opt.ubuntuMirror, p, arches)
		if err != nil {
			return nil, err
		}
		log.WithField("package", p.Binary).WithField("files", len(pool)).Info("pool listed")
		out = append(out, pool...)
		if !opt.ubuntuHistory {
			continue
		}
		inPool := map[string]bool{}
		for _, a := range pool {
			inPool[a.Name] = true
		}
		hist, err := listLaunchpad(ctx, hc, p, arches, inPool, window)
		if err != nil {
			return nil, err
		}
		log.WithField("package", p.Binary).WithField("files", len(hist)).Info("launchpad history listed")
		out = append(out, hist...)
	}
	return out, nil
}

// launchpadSeriesURL is a variable only so the tests can point it at a fake.
var launchpadSeriesURL = "https://api.launchpad.net/1.0/ubuntu/series"

// seriesWindow is the set of Ubuntu series the Launchpad history is read for.
// The zero value allows everything.
//
// Without one, libc6-dbg's history reaches back to 4.10 "warty" in 2004: 1,652
// files across 566 versions, more than half of them from releases nobody
// debugs any more, and the oldest predating the .build-id layout altogether.
type seriesWindow struct {
	series map[string]bool // nil = every series
	from   time.Time       // earliest date_created among them
}

func (w seriesWindow) allows(series string) bool {
	return w.series == nil || w.series[series]
}

// ubuntuSeriesSince reads the series list - one request, 46 entries, no
// paging - and keeps those whose version is at least since ("20.04").
//
// The pool is not filtered by this: it holds a handful of files, all of them
// live, and its listing names versions, not series.
func ubuntuSeriesSince(ctx context.Context, hc *http.Client, since string) (seriesWindow, error) {
	if since == "" {
		return seriesWindow{}, nil
	}
	floor, ok := parseSeriesVersion(since)
	if !ok {
		return seriesWindow{}, fmt.Errorf("-ubuntu-since %q: want a release version like 20.04", since)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, launchpadSeriesURL, nil)
	if err != nil {
		return seriesWindow{}, err
	}
	req.Header.Set("accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return seriesWindow{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return seriesWindow{}, fmt.Errorf("GET %s: %s", launchpadSeriesURL, resp.Status)
	}
	var page struct {
		Entries []struct {
			Name        string    `json:"name"`
			Version     string    `json:"version"`
			DateCreated time.Time `json:"date_created"`
		} `json:"entries"`
		Next string `json:"next_collection_link"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return seriesWindow{}, err
	}
	if page.Next != "" {
		// Not expected for a list of 46; failing beats silently dropping the
		// newest series, which is where the traffic is.
		return seriesWindow{}, fmt.Errorf("series list is paginated now; %s needs to follow it", launchpadSeriesURL)
	}
	w := seriesWindow{series: map[string]bool{}}
	for _, e := range page.Entries {
		v, ok := parseSeriesVersion(e.Version)
		if !ok || v < floor {
			continue
		}
		w.series[e.Name] = true
		if w.from.IsZero() || e.DateCreated.Before(w.from) {
			w.from = e.DateCreated
		}
	}
	if len(w.series) == 0 {
		return seriesWindow{}, fmt.Errorf("no Ubuntu series at or after %s", since)
	}
	return w, nil
}

// parseSeriesVersion turns "20.04" into 2004, so versions compare as numbers:
// as strings, "4.10" would sort after "20.04".
func parseSeriesVersion(v string) (int, bool) {
	var major, minor int
	if n, err := fmt.Sscanf(v, "%d.%d", &major, &minor); err != nil || n != 2 || minor < 0 || minor > 99 {
		return 0, false
	}
	return major*100 + minor, true
}
