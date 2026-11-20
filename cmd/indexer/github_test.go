package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestReleasesStopAtTheFloor: the walk ends at the first tag below since, so
// a floor costs pages back to it and no further - the archive goes back years.
func TestReleasesStopAtTheFloor(t *testing.T) {
	tags := []string{"20261110", "20261026", "20260901", "20260825", "20260814", "20260801"}
	var pages atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		var page int
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		per := 2
		lo := (page - 1) * per
		var items []string
		for i := lo; i < lo+per && i < len(tags); i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"tag_name":%q}`, i, tags[i]))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	}))
	defer ts.Close()

	g := &ghClient{hc: ts.Client(), api: ts.URL}
	got, err := g.Releases(context.Background(), "x/y", 0, "20260825")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.TagName)
	}
	if strings.Join(names, ",") != "20261110,20261026,20260901,20260825" {
		t.Fatalf("got %v", names)
	}
	// The fake serves two per page, so the first tag below the floor is on
	// page 3 - and there must be no page 4.
	if n := pages.Load(); n != 3 {
		t.Fatalf("fetched %d pages, want 3: the walk must stop at the floor", n)
	}

	got, _ = g.Releases(context.Background(), "x/y", 2, "20260825")
	if len(got) != 2 {
		t.Fatalf("max did not cap: %d releases", len(got))
	}
}

// TestFreethreadedAssetsParse: uv installs 3.13t/3.14t from these, and they
// are 60 of the 150 Linux install_only assets in a release.
func TestFreethreadedAssetsParse(t *testing.T) {
	_, triple, variant, ok := ParseAssetName("cpython-3.14.1+20261110-x86_64-unknown-linux-gnu-freethreaded-install_only.tar.gz")
	if !ok || triple != "x86_64-unknown-linux-gnu" || variant != "freethreaded-install_only" {
		t.Fatalf("got %q %q %v", triple, variant, ok)
	}
}

// fakeGitHub serves matching-refs, releases/tags and assets, and counts every
// request - the count is what these tests are about.
type fakeGitHub struct {
	tags     map[int][]string // year -> tags
	requests atomic.Int64
	paths    sync.Map
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.paths.Store(r.URL.Path, true)
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/git/matching-refs/tags/"):
			var year int
			fmt.Sscan(p[strings.LastIndex(p, "/")+1:], &year)
			var refs []string
			for _, tag := range f.tags[year] {
				refs = append(refs, fmt.Sprintf(`{"ref":"refs/tags/%s"}`, tag))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(refs, ","))
		case strings.Contains(p, "/releases/tags/"):
			tag := p[strings.LastIndex(p, "/")+1:]
			if tag == "20261201" {
				http.NotFound(w, r) // tagged, not published
				return
			}
			fmt.Fprintf(w, `{"id":%s,"tag_name":%q,"published_at":"2026-01-01T00:00:00Z"}`, tag, tag)
		case strings.Contains(p, "/assets"):
			if r.URL.Query().Get("page") != "1" {
				fmt.Fprint(w, "[]")
				return
			}
			id := strings.Split(p, "/")[5]
			fmt.Fprintf(w, `[{"name":"cpython-3.14.1+%s-x86_64-unknown-linux-gnu-install_only.tar.gz","size":1,"browser_download_url":"https://x/%s.tar.gz"},
				{"name":"cpython-3.14.1+%s-aarch64-apple-darwin-install_only.tar.gz","size":1,"browser_download_url":"https://x/mac"}]`, id, id, id)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestTagsSinceIsOneRequestPerYear(t *testing.T) {
	f := &fakeGitHub{tags: map[int][]string{
		2025: {"20251201", "20251230", "latest"},
		2026: {"20260110", "20261110"},
	}}
	ts := f.serve(t)
	g := &ghClient{hc: ts.Client(), api: ts.URL}
	got, err := g.TagsSince(context.Background(), "x/y", "20251215", time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "20261110,20260110,20251230" {
		t.Fatalf("got %v", got)
	}
	if n := f.requests.Load(); n != 2 {
		t.Fatalf("%d requests for two years of tags, want 2", n)
	}
}

// TestFinishedReleasesCostNothing is the point of indexed_releases: a run in
// steady state asks for the tag names and nothing else.
func TestFinishedReleasesCostNothing(t *testing.T) {
	f := &fakeGitHub{tags: map[int][]string{2026: {"20260825", "20261110", "20261201"}}}
	ts := f.serve(t)
	g := &ghClient{hc: ts.Client(), api: ts.URL}
	opt := options{repo: "x/y", since: "20260825", variants: []string{"install_only"}}

	done := map[string]bool{"20260825": true}
	assets, rels, err := collectAssets(context.Background(), g, opt, done)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].Release != "20261110" {
		t.Fatalf("got %+v; want only the open, published release's linux asset", assets)
	}
	if len(rels) != 1 || len(rels[0].URLs) != 1 {
		t.Fatalf("listed %+v", rels)
	}
	if _, ok := f.paths.Load("/repos/x/y/releases/tags/20260825"); ok {
		t.Fatal("a finished release was looked up")
	}

	f.requests.Store(0)
	done["20261110"] = true
	if _, _, err := collectAssets(context.Background(), g, opt, done); err != nil {
		t.Fatal(err)
	}
	// matching-refs, plus the unpublished tag, which is still open.
	if n := f.requests.Load(); n != 2 {
		t.Fatalf("%d requests with every published release finished, want 2", n)
	}
}

type memStore struct {
	state map[string]archiveState
	done  []string
}

func (m *memStore) LoadState(context.Context) (map[string]archiveState, error) { return m.state, nil }
func (m *memStore) InsertBatch(context.Context, []artifactRow, []archiveRow) error {
	return nil
}
func (m *memStore) DoneReleases(context.Context, string) (map[string]bool, error) {
	return nil, nil
}
func (m *memStore) MarkReleasesDone(_ context.Context, _ string, tags []string) error {
	m.done = append(m.done, tags...)
	return nil
}

func TestCloseReleasesOnlyWhenCompleteAndSettled(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := &memStore{state: map[string]archiveState{"a1": {}, "a2": {}, "b1": {}, "c1": {}}}
	rels := []pbsRelease{
		{Tag: "complete", Published: now.Add(-48 * time.Hour), URLs: []string{"a1", "a2"}},
		{Tag: "missing-one", Published: now.Add(-48 * time.Hour), URLs: []string{"b1", "b2"}},
		{Tag: "too-fresh", Published: now.Add(-time.Hour), URLs: []string{"c1"}},
	}
	if err := closeReleases(context.Background(), st, options{variants: []string{"install_only"}}, rels, now); err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.done, ",") != "complete" {
		t.Fatalf("closed %v, want only the complete, settled release", st.done)
	}
}

func TestVariantsKeyIgnoresOrder(t *testing.T) {
	if variantsKey([]string{"freethreaded-install_only", " install_only"}) != variantsKey([]string{"install_only", "freethreaded-install_only"}) {
		t.Fatal("the same variant set must close the same releases")
	}
}
