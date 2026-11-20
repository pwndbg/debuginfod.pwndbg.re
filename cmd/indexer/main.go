// Command indexer records which downloadable archive holds the debug info for a
// build id that no distribution debuginfod serves. cmd/index-debuginfod serves
// from what it writes.
//
// Two sources, both gaps measured rather than assumed:
//
//   - pbs: python-build-standalone, where uv gets its Pythons. uv installs the
//     install_only_stripped tarballs; those binaries keep .note.gnu.build-id but
//     carry no DWARF, and no distribution knows the ids, because these are
//     astral's builds. The install_only asset of the same release holds the same
//     binaries unstripped, under the same paths and with the same build ids.
//
//   - ubuntu: packages Ubuntu ships debug info for as a plain .deb, which
//     debuginfod.ubuntu.com does not index - libc6-dbg above all, so the libc of
//     every Ubuntu since 18.04 is a 404 there. See ubuntu.go.
//
//     indexer                                  # both sources, once
//     indexer -every 6h                        # both sources, forever
//     indexer -sources ubuntu -ubuntu-history  # backfill every libc6-dbg Launchpad has
//     indexer -sources pbs -releases 2         # only the two newest python releases
//     indexer -sources pbs -tag 20260825 -dry-run   # JSONL on stdout, no ClickHouse
//
// Set GITHUB_TOKEN for pbs: the API allows 60 unauthenticated requests an hour
// and one release needs nine pages of asset listings.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/pwndbg/debuginfod.pwndbg.re/useragent"
)

// store is what the indexer writes through. *dbSrv is the real one; jsonStore
// backs -dry-run, and both keep the tests free of ClickHouse.
type store interface {
	LoadState(ctx context.Context) (map[string]archiveState, error)
	InsertBatch(ctx context.Context, arts []artifactRow, archives []archiveRow) error
	DoneReleases(ctx context.Context, variants string) (map[string]bool, error)
	MarkReleasesDone(ctx context.Context, variants string, tags []string) error
}

type options struct {
	sources map[string]bool

	// pbs
	repo     string
	tag      string
	since    string
	releases int
	variants []string

	// ubuntu
	ubuntuMirror   string
	ubuntuPackages string
	ubuntuArches   string
	ubuntuHistory  bool
	ubuntuSince    string

	workers int
	batch   int
	dsn     string
	dryRun  bool
	force   bool
	limit   int
	every   time.Duration
}

func main() {
	var opt options
	var variants, sources string
	flag.StringVar(&sources, "sources", "ubuntu,pbs", "comma-separated sources to index: ubuntu, pbs")
	flag.StringVar(&opt.repo, "repo", defaultRepo, "pbs: GitHub repo to index")
	flag.StringVar(&opt.tag, "tag", "", "pbs: index only this release tag (default: newest -releases)")
	flag.StringVar(&opt.since, "since", firstReleaseWithBuildIDs, "pbs: index every release from this tag on (empty = no floor)")
	flag.IntVar(&opt.releases, "releases", 0, "pbs: index at most this many recent releases (0 = no cap)")
	flag.StringVar(&variants, "variants", "install_only,freethreaded-install_only", "pbs: comma-separated asset variants to scan")
	flag.StringVar(&opt.ubuntuMirror, "ubuntu-mirror", "https://archive.ubuntu.com/ubuntu", "ubuntu: archive mirror")
	flag.StringVar(&opt.ubuntuPackages, "ubuntu-packages", "main/glibc/libc6-dbg", "ubuntu: comma-separated component/source/binary")
	flag.StringVar(&opt.ubuntuArches, "ubuntu-arches", "amd64,amd64v3,i386,arm64,armhf", "ubuntu: architectures to index")
	flag.BoolVar(&opt.ubuntuHistory, "ubuntu-history", false, "ubuntu: also index every version Launchpad ever published, not just the pool")
	flag.StringVar(&opt.ubuntuSince, "ubuntu-since", "20.04", "ubuntu: read the Launchpad history only for this release and newer (empty = all, back to 2004)")
	flag.IntVar(&opt.workers, "workers", 4, "how many archives to download at once")
	flag.IntVar(&opt.batch, "batch", 2000, "rows per ClickHouse insert")
	flag.StringVar(&opt.dsn, "dsn", envOr("CLICKHOUSE_DSN", "clickhouse://127.0.0.1:9000?username=cypis&password=cypis"), "ClickHouse DSN")
	flag.BoolVar(&opt.dryRun, "dry-run", false, "write JSONL to stdout instead of ClickHouse")
	flag.BoolVar(&opt.force, "force", false, "re-scan archives the resume log says are done")
	flag.IntVar(&opt.limit, "limit", 0, "stop after this many archives (0 = no limit)")
	flag.DurationVar(&opt.every, "every", 0, "repeat the run at this interval (0 = run once and exit)")
	logLevel := flag.String("log-level", envOr("LOG_LEVEL", "info"), "trace|debug|info|warn|error")
	flag.Parse()

	applyLogLevel(*logLevel)
	opt.variants = strings.Split(variants, ",")
	opt.sources = map[string]bool{}
	for _, s := range strings.Split(sources, ",") {
		switch s = strings.TrimSpace(s); s {
		case sourcePBS, sourceUbuntu:
			opt.sources[s] = true
		case "":
		default:
			log.WithField("source", s).Fatal("unknown source")
		}
	}
	if opt.workers < 1 {
		opt.workers = 1
	}

	// SIGINT cancels rather than kills: whatever is already batched still gets
	// written, so a long run can be stopped and resumed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var st store
	if opt.dryRun {
		st = &jsonStore{enc: json.NewEncoder(os.Stdout)}
	} else {
		db, err := NewDB(ctx, opt.dsn)
		if err != nil {
			log.WithError(err).Fatal("connecting db")
		}
		defer db.Close()
		if err := db.Init(ctx); err != nil {
			log.WithError(err).Fatal("initializing db")
		}
		st = db
	}

	for {
		err := run(ctx, st, opt)
		if err == nil && opt.ubuntuHistory {
			// Launchpad's history is needed once. After it, every version is
			// indexed - and every pool file also under its Launchpad url, so it
			// outlives the pool - and anything new appears in the pool first.
			// A restart pays it again: ~20 requests, not worth a table.
			opt.ubuntuHistory = false
			log.Info("launchpad history indexed; later runs list the pool only")
		}
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		if opt.every <= 0 {
			if err != nil {
				log.WithError(err).Fatal("indexing failed")
			}
			return
		}
		// In a loop a failed run is logged, never fatal: the next one usually
		// succeeds, and a restart loop would hammer the same upstream harder.
		if err != nil {
			log.WithError(err).Error("indexing run failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(opt.every):
		}
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// result is one finished archive on its way to the writer.
type result struct {
	archive archiveRow
	arts    []artifactRow
}

type counters struct {
	scanned, skipped, failed, gone, artifacts atomic.Int64
	// processed and total give the "progress" field: every archive taken off
	// the queue counts, whatever its outcome.
	processed, total atomic.Int64
}

// collect lists every archive the selected sources offer. A source that cannot
// be listed is logged and skipped rather than failing the run: GitHub's rate
// limit must not cost the Ubuntu half, or the other way round.
//
// It also returns the python-build-standalone releases it had to look into,
// so the run can close the ones it finished.
func collect(ctx context.Context, hc *http.Client, opt options, done map[string]bool) ([]Archive, []pbsRelease, error) {
	var out []Archive
	var rels []pbsRelease
	var errs []error
	if opt.sources[sourceUbuntu] {
		ub, err := collectUbuntu(ctx, hc, opt)
		if err != nil {
			errs = append(errs, fmt.Errorf("ubuntu: %w", err))
		}
		out = append(out, ub...)
	}
	if opt.sources[sourcePBS] {
		gh := newGHClient()
		if gh.token == "" {
			log.Warn("GITHUB_TOKEN is unset: the API allows 60 requests an hour, which one release will exhaust")
		}
		assets, listed, err := collectAssets(ctx, gh, opt, done)
		if err != nil {
			errs = append(errs, fmt.Errorf("pbs: %w", err))
		}
		for _, a := range assets {
			out = append(out, a.Archive())
		}
		rels = listed
	}
	return out, rels, errors.Join(errs...)
}

func run(ctx context.Context, st store, opt options) error {
	// No overall client timeout: an asset is hundreds of megabytes and a slow
	// mirror is not an error. Cancellation comes from ctx instead.
	hc := useragent.Client(&http.Client{}, "indexer")

	state := map[string]archiveState{}
	done := map[string]bool{}
	if !opt.force && !opt.dryRun {
		var err error
		if state, err = st.LoadState(ctx); err != nil {
			return fmt.Errorf("loading resume state: %w", err)
		}
		if done, err = st.DoneReleases(ctx, variantsKey(opt.variants)); err != nil {
			return fmt.Errorf("loading finished releases: %w", err)
		}
		log.WithField("known", len(state)).WithField("releases_done", len(done)).Info("resume state loaded")
	}

	archives, rels, listErr := collect(ctx, hc, opt, done)
	if listErr != nil {
		log.WithError(listErr).Error("listing failed for a source; indexing the rest")
	}
	log.WithField("archives", len(archives)).Info("archives selected")

	var c counters
	work := make(chan Archive)
	results := make(chan result)

	g, ctx := errgroup.WithContext(ctx)

	// Decide what is left before starting, so the run can say how much there
	// is - a first run is hours, and "scanned" lines alone do not say how many
	// remain.
	var todo []Archive
	left := map[string]int{}
	for _, a := range archives {
		if prev, ok := state[a.URL]; ok && (a.Size == 0 || prev.Size == uint64(a.Size)) {
			c.skipped.Add(1)
			continue
		}
		todo = append(todo, a)
		left[a.Source]++
	}
	if opt.limit > 0 && len(todo) > opt.limit {
		todo = todo[:opt.limit]
	}
	c.total.Store(int64(len(todo)))
	log.WithField("todo", len(todo)).WithField("by_source", left).
		WithField("already_done", c.skipped.Load()).Info("archives to index")

	g.Go(func() error {
		defer close(work)
		for _, a := range todo {
			select {
			case work <- a:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})

	var wg sync.WaitGroup
	for range opt.workers {
		wg.Add(1)
		g.Go(func() error {
			defer wg.Done()
			for a := range work {
				r := scanOne(ctx, hc, a, &c)
				select {
				case results <- r:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	g.Go(func() error { return writeResults(ctx, st, opt, results, &c) })

	err := g.Wait()
	if ctx.Err() == nil && !opt.dryRun && len(rels) > 0 {
		if cerr := closeReleases(context.WithoutCancel(ctx), st, opt, rels, time.Now()); cerr != nil {
			log.WithError(cerr).Warn("could not record finished releases; they will be listed again")
		}
	}
	log.WithField("scanned", c.scanned.Load()).
		WithField("skipped", c.skipped.Load()).
		WithField("failed", c.failed.Load()).
		WithField("gone", c.gone.Load()).
		WithField("buildids", c.artifacts.Load()).
		Info("done")
	return errors.Join(err, listErr)
}

// pbsRelease is a release this run looked into, with the urls it selected -
// what closeReleases needs to decide whether it is finished.
type pbsRelease struct {
	Tag       string
	Published time.Time
	URLs      []string
}

// collectAssets turns the requested releases into the list of assets worth
// downloading. Filtering happens entirely on the asset names, so a release of
// several hundred assets costs a few API calls and no transfer - and a release
// already closed costs none at all.
func collectAssets(ctx context.Context, gh *ghClient, opt options, done map[string]bool) ([]Asset, []pbsRelease, error) {
	var releases []ghRelease
	switch {
	case opt.tag != "":
		// Asked for by name: listed even when closed.
		rel, err := gh.ReleaseByTag(ctx, opt.repo, opt.tag)
		if err != nil {
			return nil, nil, err
		}
		releases = []ghRelease{rel}
	case opt.since != "":
		tags, err := gh.TagsSince(ctx, opt.repo, opt.since, time.Now())
		if err != nil {
			return nil, nil, err
		}
		if opt.releases > 0 && len(tags) > opt.releases {
			tags = tags[:opt.releases]
		}
		open := 0
		for _, tag := range tags {
			if done[tag] {
				continue
			}
			open++
			rel, err := gh.ReleaseByTag(ctx, opt.repo, tag)
			if errors.Is(err, errGHNotFound) {
				// Tagged, not yet published. The next run will see it.
				log.WithField("release", tag).Info("tag has no release yet")
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			releases = append(releases, rel)
		}
		log.WithField("tags", len(tags)).WithField("open", open).Info("python-build-standalone releases listed")
	default:
		var err error
		if releases, err = gh.Releases(ctx, opt.repo, opt.releases, ""); err != nil {
			return nil, nil, err
		}
	}

	wanted := map[string]bool{}
	for _, v := range opt.variants {
		wanted[strings.TrimSpace(v)] = true
	}

	var out []Asset
	var listed []pbsRelease
	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		raw, err := gh.Assets(ctx, opt.repo, rel.ID)
		if err != nil {
			return out, listed, fmt.Errorf("assets of %s: %w", rel.TagName, err)
		}
		pr := pbsRelease{Tag: rel.TagName, Published: rel.PublishedAt}
		for _, a := range raw {
			python, triple, variant, ok := ParseAssetName(a.Name)
			if !ok || !IsELFTarget(triple) || !wanted[variant] {
				continue
			}
			out = append(out, Asset{
				Release: rel.TagName, Name: a.Name, URL: a.URL, Size: a.Size,
				Python: python, Triple: triple, Variant: variant,
			})
			pr.URLs = append(pr.URLs, a.URL)
		}
		listed = append(listed, pr)
		log.WithField("release", rel.TagName).
			WithField("assets", len(raw)).WithField("selected", len(pr.URLs)).Debug("release listed")
	}
	return out, listed, nil
}

// releaseSettle is how long after publishing a release is still allowed to
// change. Assets of a large release are uploaded over time, and closing one
// that is still filling in would leave the rest unindexed for good.
const releaseSettle = 24 * time.Hour

// closeReleases records the releases whose every selected asset is now in the
// resume log, so later runs do not list them at all. A release with a failed
// asset stays open and is retried; "gone" counts as finished, as it does for a
// single archive.
func closeReleases(ctx context.Context, st store, opt options, rels []pbsRelease, now time.Time) error {
	state, err := st.LoadState(ctx)
	if err != nil {
		return err
	}
	var finished []string
	for _, r := range rels {
		if r.Published.IsZero() || now.Sub(r.Published) < releaseSettle {
			continue
		}
		complete := true
		for _, u := range r.URLs {
			if _, ok := state[u]; !ok {
				complete = false
				break
			}
		}
		if complete {
			finished = append(finished, r.Tag)
		}
	}
	if len(finished) == 0 {
		return nil
	}
	log.WithField("releases", finished).Info("python-build-standalone releases finished")
	return st.MarkReleasesDone(ctx, variantsKey(opt.variants), finished)
}

// variantsKey: a release is finished FOR A SET OF VARIANTS. Adding one later
// must reopen every release, or the new variant would never be indexed.
func variantsKey(variants []string) string {
	vs := make([]string, 0, len(variants))
	for _, v := range variants {
		if v = strings.TrimSpace(v); v != "" {
			vs = append(vs, v)
		}
	}
	sort.Strings(vs)
	return strings.Join(vs, ",")
}

func scanOne(ctx context.Context, hc *http.Client, a Archive, c *counters) result {
	started := time.Now()
	r := result{archive: archiveRow{Archive: a}}
	progress := func() string {
		return fmt.Sprintf("%d/%d", c.processed.Add(1), c.total.Load())
	}

	arts, err := ScanArchive(ctx, hc, a)
	if err != nil {
		// One bad archive is logged and recorded, never fatal: a truncated
		// download must not cost the rest of the run.
		switch {
		case errors.Is(err, context.Canceled):
		case errors.Is(err, errGone):
			log.WithField("archive", a.URL).WithField("progress", progress()).Info("archive is gone")
			c.gone.Add(1)
		default:
			log.WithError(err).WithField("archive", a.Name).WithField("progress", progress()).Warn("scanning archive failed")
			c.failed.Add(1)
		}
		r.archive.Err = err.Error()
		return r
	}

	r.archive.BuildIDs = uint32(len(arts))
	for _, art := range arts {
		r.arts = append(r.arts, artifactRow{Artifact: art, Archive: a})
		if a.Fallback != "" {
			alt := a
			alt.URL, alt.Fallback = a.Fallback, ""
			r.arts = append(r.arts, artifactRow{Artifact: art, Archive: alt})
		}
	}
	c.scanned.Add(1)
	c.artifacts.Add(int64(len(arts)))
	log.WithField("archive", a.Name).WithField("buildids", len(arts)).WithField("progress", progress()).
		WithField("took", time.Since(started).Round(time.Millisecond)).Info("scanned")
	return r
}

func writeResults(ctx context.Context, st store, opt options, results <-chan result, c *counters) error {
	var (
		arts     []artifactRow
		archives []archiveRow
		flushErr error
	)
	flush := func() error {
		if len(arts) == 0 && len(archives) == 0 {
			return nil
		}
		// A cancelled ctx must not stop the final flush - draining on SIGINT is
		// the point - so the write gets its own deadline.
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		err := st.InsertBatch(wctx, arts, archives)
		arts, archives = arts[:0], archives[:0]
		return err
	}

	for r := range results {
		// A cancelled scan is not a result. Recording it would write an error
		// row for an archive that was simply interrupted.
		if errors.Is(ctx.Err(), context.Canceled) && r.archive.Err != "" {
			continue
		}
		arts = append(arts, r.arts...)
		archives = append(archives, r.archive)
		if len(arts) >= opt.batch || len(archives) >= opt.batch {
			if err := flush(); err != nil {
				flushErr = err
				break
			}
		}
	}
	// Drain, so an error or a signal does not throw away completed work.
	for range results {
	}
	if flushErr != nil {
		return flushErr
	}
	return flush()
}

// jsonStore backs -dry-run: one JSON object per archive, so the indexer can be
// pointed at a source without being pointed at a live database.
type jsonStore struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (j *jsonStore) LoadState(context.Context) (map[string]archiveState, error) {
	return map[string]archiveState{}, nil
}

func (j *jsonStore) DoneReleases(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (j *jsonStore) MarkReleasesDone(context.Context, string, []string) error { return nil }

func (j *jsonStore) InsertBatch(_ context.Context, arts []artifactRow, archives []archiveRow) error {
	byURL := map[string][]Artifact{}
	for _, a := range arts {
		byURL[a.Archive.URL] = append(byURL[a.Archive.URL], a.Artifact)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, a := range archives {
		rec := struct {
			Source    string     `json:"source"`
			Name      string     `json:"name"`
			URL       string     `json:"url"`
			Header    string     `json:"archive"`
			Kind      string     `json:"kind"`
			Error     string     `json:"error,omitempty"`
			Artifacts []Artifact `json:"artifacts,omitempty"`
		}{
			Source: a.Archive.Source, Name: a.Archive.Name, URL: a.Archive.URL,
			Header: a.Archive.Header, Kind: a.Archive.Kind, Error: a.Err, Artifacts: byURL[a.Archive.URL],
		}
		if err := j.enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}
