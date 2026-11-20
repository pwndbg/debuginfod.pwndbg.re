package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pwndbg/debuginfod.pwndbg.re/useragent"
)

const (
	githubAPI = "https://api.github.com"

	// defaultRepo is where uv gets its Pythons. Every build id a `uv python
	// install` puts on a machine was produced by one of these releases.
	defaultRepo = "astral-sh/python-build-standalone"
)

type ghRelease struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

// errGHNotFound: the API answered 404 - for a tag, "no release (yet)".
var errGHNotFound = errors.New("not found")

type ghAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

type ghClient struct {
	hc    *http.Client
	token string
	api   string // githubAPI, or a fake in tests
}

func newGHClient() *ghClient {
	return &ghClient{
		hc: useragent.Client(&http.Client{Timeout: 60 * time.Second}, "indexer"),
		// Unauthenticated the API allows 60 requests an hour, and one release
		// alone needs nine pages of assets. A token raises that to 5000, which
		// is the difference between indexing the archive and not.
		token: os.Getenv("GITHUB_TOKEN"),
		api:   githubAPI,
	}
}

func (g *ghClient) get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/vnd.github+json")
	if g.token != "" {
		req.Header.Set("authorization", "Bearer "+g.token)
	}

	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("GET %s: %w", url, errGHNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		// The remaining-quota header is the one thing worth naming in the
		// error: a 403 here is almost always the rate limit rather than auth,
		// and the two are otherwise indistinguishable.
		return fmt.Errorf("GET %s: %s (x-ratelimit-remaining: %q)",
			url, resp.Status, resp.Header.Get("x-ratelimit-remaining"))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Releases lists releases newest first, stopping after max of them (0 = no
// cap) or at the first tag older than since ("" = no floor), whichever comes
// first.
//
// since compares tags as strings. python-build-standalone tags are YYYYMMDD,
// so that is chronological, and the list comes newest first, so the first tag
// below the floor ends the walk without paging through years of releases.
func (g *ghClient) Releases(ctx context.Context, repo string, max int, since string) ([]ghRelease, error) {
	// Small pages, not 100. Every release object inlines its assets - several
	// hundred each here - so a page of 100 is tens of megabytes that GitHub
	// assembles before answering, and it answers 504 instead. Measured.
	perPage := 10
	if max > 0 && max < perPage {
		perPage = max
	}
	var out []ghRelease
	for page := 1; ; page++ {
		var batch []ghRelease
		url := fmt.Sprintf("%s/repos/%s/releases?per_page=%d&page=%d", g.api, repo, perPage, page)
		if err := g.get(ctx, url, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		for _, r := range batch {
			if r.Draft {
				continue
			}
			if since != "" && r.TagName < since {
				return out, nil
			}
			out = append(out, r)
			if max > 0 && len(out) >= max {
				return out, nil
			}
		}
	}
}

// Assets lists one release's assets.
//
// Deliberately its own request rather than the assets array embedded in the
// release object: a python-build-standalone release carries several hundred
// assets, well past what the list endpoint inlines, so reading the embedded
// array would index a silently truncated set - the failure would look like
// "these build ids do not exist" rather than like an error.
func (g *ghClient) Assets(ctx context.Context, repo string, releaseID int64) ([]ghAsset, error) {
	var out []ghAsset
	for page := 1; ; page++ {
		var batch []ghAsset
		url := fmt.Sprintf("%s/repos/%s/releases/%d/assets?per_page=100&page=%d",
			g.api, repo, releaseID, page)
		if err := g.get(ctx, url, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		out = append(out, batch...)
	}
}

// firstReleaseWithBuildIDs is where python-build-standalone indexing starts.
//
// Before it, the Linux builds carry no .note.gnu.build-id at all, so a
// debugger has nothing to ask with and nothing older could ever be requested
// by id - indexing it would download gigabytes to record nothing.
// astral-sh/python-build-standalone#1230 ("Emit GNU build IDs on Linux
// targets", -Wl,--build-id=sha1) merged 2026-08-25 13:31 UTC, and 20260825,
// published two hours later, is the first release that contains it.
//
// One exception, deliberately not chased: aarch64 had build ids up to 20250630
// while it was cross-compiled with a host toolchain, and lost them at 20250702.
const firstReleaseWithBuildIDs = "20260825"

var dateTag = regexp.MustCompile(`^[0-9]{8}$`)

// TagsSince lists release tags from since on, newest first, without touching
// a single release or asset.
//
// This is what keeps a periodic run cheap. The release list inlines several
// hundred assets per release, so walking it every few hours re-downloads
// megabytes describing files that were indexed long ago. git/matching-refs
// returns tag NAMES only, every tag with a prefix in one unpaginated response -
// and tags here are YYYYMMDD, so one prefix per calendar year covers the range:
// a run in steady state is one request.
func (g *ghClient) TagsSince(ctx context.Context, repo, since string, now time.Time) ([]string, error) {
	if len(since) < 4 {
		return nil, fmt.Errorf("since %q is not a YYYYMMDD tag", since)
	}
	var first int
	if _, err := fmt.Sscan(since[:4], &first); err != nil {
		return nil, fmt.Errorf("since %q is not a YYYYMMDD tag", since)
	}
	var out []string
	for year := first; year <= now.Year(); year++ {
		var refs []struct {
			Ref string `json:"ref"`
		}
		url := fmt.Sprintf("%s/repos/%s/git/matching-refs/tags/%d", g.api, repo, year)
		if err := g.get(ctx, url, &refs); err != nil {
			return nil, err
		}
		for _, r := range refs {
			tag := strings.TrimPrefix(r.Ref, "refs/tags/")
			if dateTag.MatchString(tag) && tag >= since {
				out = append(out, tag)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// ReleaseByTag fetches one release. errGHNotFound when the tag exists but its
// release does not - pushed, not yet published.
func (g *ghClient) ReleaseByTag(ctx context.Context, repo, tag string) (ghRelease, error) {
	var rel ghRelease
	err := g.get(ctx, fmt.Sprintf("%s/repos/%s/releases/tags/%s", g.api, repo, tag), &rel)
	return rel, err
}
