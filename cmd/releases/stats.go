package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/julienschmidt/httprouter"
)

type labelCount struct {
	Label string
	N     uint64
}

// releaseStats is one finished window. Unlike cmd/proxy, which collects the
// longest window once and cuts shorter views out of the tail, every view here is
// collected separately: the headline number is a distinct-IP count, and distinct
// counts cannot be re-aggregated from daily ones (the same client on two days is
// one client, not two). The data is small enough that running the queries per view
// costs less than carrying uniq states around would.
type releaseStats struct {
	Days      []string
	Redirects []uint64 // per day, status 302
	Rejected  []uint64 // per day, anything else
	IPsPerDay []uint64

	Total         uint64
	UniqIPs       uint64
	UniqCountries uint64

	Platforms []labelCount
	Variants  []labelCount
	Formats   []labelCount
	Assets    []labelCount
	Countries []labelCount
	Clients   []labelCount
	Versions  []labelCount

	Curves        []releaseCurve
	CurveDays     int
	GhDaily       []uint64
	GhLatestTag   string
	GhLatestTotal uint64
	GhTotal       uint64

	Window      int
	Views       []int
	GeneratedAt time.Time
	Took        time.Duration
}

// releaseCurve is one release's cumulative downloads, indexed by days since we
// first saw it - which is what makes two releases comparable at all. A total is
// not: the counter is only polled while a tag is `latest`, so it stops growing
// when the successor ships, and an all-time figure mostly measures how long the
// release stayed current.
type releaseCurve struct {
	Tag       string
	Series    []float64 // cumulative, baseline at day 0 removed, monotonic
	Total     uint64    // last value observed
	AtHorizon uint64    // value at curveHorizon days, 0 if never reached
	Reached   bool      // sampled long enough to have an AtHorizon
}

// curveHorizon is how far a release is followed. Every curve is drawn over the
// same first 30 days, which is the window where a release is actually being
// picked up; past that the lines only record how long each tag stayed current.
const curveHorizon = 30

// maxCurves caps how many releases are drawn. The palette has five validated
// hues and a legend stops being readable well before that.
const maxCurves = 5

type statsSource interface {
	CollectStats(ctx context.Context, days int) (*releaseStats, error)
}

const statsQueryTimeout = 60 * time.Second

// releaseRows is the row filter every bucket query shares. status = 302 keeps a
// request that never became a download - a foreign Host, a malformed path - out
// of the counts.
const releaseRows = `timestamp >= today() - ? AND status = 302 AND file != ''`

// clientBucket is the classification the Grafana dashboard has been using against
// access_log, so the numbers here stay comparable with the ones read there before
// the rows moved. Homebrew puts the architecture in its User-Agent, which is the
// only place a Homebrew user's platform is knowable - the formula picks the
// bottle, so the asset name does not say.
//
// Closed vocabulary. The split worth having is installer vs Homebrew; a raw
// User-Agent is never used as a label, so nothing a client sends can name a bar.
const clientBucket = `multiIf(
    positionCaseInsensitive(user_agent, 'Homebrew') > 0 AND positionCaseInsensitive(user_agent, 'arm64') > 0, 'Homebrew arm64',
    positionCaseInsensitive(user_agent, 'Homebrew') > 0, 'Homebrew intel',
    positionCaseInsensitive(user_agent, 'pwndbg-installer') > 0, 'pwndbg-installer',
    'other')`

// versionBucket keeps only version-shaped tags as their own bar. The value comes
// straight out of the request path, so without it any string a client puts there
// becomes a row on this page.
//
// The dots are escaped on purpose: an unescaped '.' matches any character, which
// would let "2026x02y18" through as a version. [0-9] rather than \d so nothing
// depends on how the driver treats a backslash.
const versionBucket = `if(match(version, '^[0-9]+\.[0-9]+\.[0-9]+$'), version, 'other')`

func (s *dbSrv) CollectStats(ctx context.Context, days int) (*releaseStats, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, statsQueryTimeout)
	defer cancel()

	snap := &releaseStats{GeneratedAt: time.Now(), Window: days}

	// Built from the clock, not from the data: a day with no downloads has to stay
	// a gap in the chart rather than vanish and pull its neighbours together.
	end := time.Now().UTC().Truncate(24 * time.Hour)
	idx := make(map[string]int, days+1)
	for i := days; i >= 0; i-- {
		d := end.AddDate(0, 0, -i).Format("2006-01-02")
		idx[d] = len(snap.Days)
		snap.Days = append(snap.Days, d)
	}
	n := len(snap.Days)
	snap.Redirects = make([]uint64, n)
	snap.Rejected = make([]uint64, n)
	snap.IPsPerDay = make([]uint64, n)
	snap.GhDaily = make([]uint64, n)

	for _, step := range []struct {
		name string
		fn   func(context.Context, *releaseStats, map[string]int, int) error
	}{
		{"daily", s.collectDaily},
		{"totals", s.collectTotals},
		{"dimensions", s.collectDimensions},
		{"github", s.collectGitHub},
		{"assets", s.collectAssets},
		{"curves", s.collectCurves},
	} {
		if err := step.fn(ctx, snap, idx, days); err != nil {
			return nil, fmt.Errorf("%s: %w", step.name, err)
		}
	}

	snap.Took = time.Since(started)
	return snap, nil
}

func (s *dbSrv) collectDaily(ctx context.Context, snap *releaseStats, idx map[string]int, days int) error {
	const query = `
		SELECT toDate(timestamp) AS d,
		       countIf(status = 302) AS ok,
		       countIf(status != 302) AS bad,
		       uniqIf(remote_ip, status = 302) AS ips
		FROM releases_access_log
		WHERE timestamp >= today() - ?
		GROUP BY d
	`
	rows, err := s.conn.Query(ctx, query, days)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var d time.Time
		var ok, bad, ips uint64
		if err := rows.Scan(&d, &ok, &bad, &ips); err != nil {
			return err
		}
		if i, found := idx[d.Format("2006-01-02")]; found {
			snap.Redirects[i], snap.Rejected[i], snap.IPsPerDay[i] = ok, bad, ips
		}
	}
	return rows.Err()
}

func (s *dbSrv) collectTotals(ctx context.Context, snap *releaseStats, _ map[string]int, days int) error {
	const query = `
		SELECT count(), uniq(remote_ip), uniqIf(country, country != '')
		FROM releases_access_log
		WHERE ` + releaseRows + `
	`
	return s.conn.QueryRow(ctx, query, days).
		Scan(&snap.Total, &snap.UniqIPs, &snap.UniqCountries)
}

// collectAssets breaks the current release down by artifact, from GitHub's
// counters rather than from our own log.
//
// This has to come from GitHub: releases_access_log only sees clients that went
// through this host - the install script and Homebrew - and anyone who opens the
// releases page and clicks a file never appears in it. Asking our log which
// artifact people take would answer a different, much narrower question, and
// would quietly read as if it were the whole picture.
//
// Restricted to the release that is currently `latest` on purpose: every asset in
// one release has been counted over the same span, which is what makes them
// comparable. Summed across releases they would not be - an older tag stopped
// being polled when its successor shipped.
//
// The buckets are derived in Go so the split is unit-tested and can change
// without rewriting stored rows. Cardinality is one row per asset per release.
func (s *dbSrv) collectAssets(ctx context.Context, snap *releaseStats, _ map[string]int, _ int) error {
	const query = `
		SELECT asset_name, toUInt64(max(download_count)) AS n
		FROM github_download_stats
		WHERE release_tag = (SELECT argMax(release_tag, timestamp) FROM github_download_stats)
		GROUP BY asset_name
		ORDER BY n DESC
	`
	rows, err := s.conn.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	plat, variant, format := map[string]uint64{}, map[string]uint64{}, map[string]uint64{}
	for rows.Next() {
		var name string
		var n uint64
		if err := rows.Scan(&name, &n); err != nil {
			return err
		}
		c := classifyAsset(name)
		plat[c.Platform] += n
		variant[c.Variant] += n
		format[c.Format] += n
		snap.Assets = append(snap.Assets, labelCount{name, n})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	snap.Assets = topN(snap.Assets, 10)
	snap.Platforms = topN(mapToCounts(plat), 8)
	snap.Variants = topN(mapToCounts(variant), 6)
	snap.Formats = topN(mapToCounts(format), 6)
	return nil
}

func (s *dbSrv) collectDimensions(ctx context.Context, snap *releaseStats, _ map[string]int, days int) error {
	for _, q := range []struct {
		expr string
		agg  string
		dst  *[]labelCount
		keep int
	}{
		{`if(country = '', 'unknown', country)`, "count()", &snap.Countries, 12},
		{versionBucket, "count()", &snap.Versions, 8},
		// uniq(remote_ip), not count(): an installer that retries or resumes
		// would otherwise look like several users. Buckets do not add up to the
		// headline distinct-IP count - one address can appear in two of them.
		{clientBucket, "uniq(remote_ip)", &snap.Clients, 10},
	} {
		query := `
			SELECT ` + q.expr + ` AS k, ` + q.agg + ` AS n
			FROM releases_access_log
			WHERE ` + releaseRows + `
			GROUP BY k
			ORDER BY n DESC
		`
		rows, err := s.conn.Query(ctx, query, days)
		if err != nil {
			return err
		}
		var out []labelCount
		for rows.Next() {
			var k string
			var n uint64
			if err := rows.Scan(&k, &n); err != nil {
				rows.Close()
				return err
			}
			out = append(out, labelCount{k, n})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		*q.dst = topN(out, q.keep)
	}
	return nil
}

// collectGitHub reads the other, unrelated source on this page: GitHub's own
// download counters. They count real asset downloads from everywhere, including
// people who never touched this host - which is why they are kept in their own
// section rather than mixed into the redirect numbers.
func (s *dbSrv) collectGitHub(ctx context.Context, snap *releaseStats, idx map[string]int, days int) error {
	const grandTotal = `
		SELECT sum(mx) FROM (
			SELECT release_tag, asset_name, max(download_count) AS mx
			FROM github_download_stats
			GROUP BY release_tag, asset_name
		)
	`
	if err := s.conn.QueryRow(ctx, grandTotal).Scan(&snap.GhTotal); err != nil {
		log.WithError(err).Debug("stats: no github snapshot yet")
	}

	// The counter is only sampled for whatever release is currently `latest`, so a
	// tag stops accumulating here the moment a newer one ships even though people
	// keep downloading it. Any "downloads per day" is therefore about the current
	// release, and older tags are an undercount.
	const latestTag = `
		SELECT release_tag, sum(mx) FROM (
			SELECT release_tag, asset_name, max(download_count) AS mx
			FROM github_download_stats
			WHERE release_tag = (SELECT argMax(release_tag, timestamp) FROM github_download_stats)
			GROUP BY release_tag, asset_name
		) GROUP BY release_tag
	`
	if err := s.conn.QueryRow(ctx, latestTag).Scan(&snap.GhLatestTag, &snap.GhLatestTotal); err != nil {
		// No rows at all - the collector has never run. Not fatal for the page.
		log.WithError(err).Debug("stats: no github snapshot yet")
		return nil
	}

	// Per-day downloads have to be differenced out of the cumulative counter.
	//
	// prev_ts guards the first sample of each (tag, asset): lagInFrame returns the
	// column default there, so without it the first row would report the release's
	// entire download history as a single day's traffic. The same guard is what
	// keeps a new tag from producing a spike. It also means the hours between a
	// release being published and our first poll are dropped rather than
	// attributed - an undercount at exactly the busiest moment of a release.
	//
	// delta > 0 drops the rest: GitHub occasionally revises a counter downwards,
	// and a gap in polling makes one sample cover many hours.
	const perDay = `
		SELECT d, sum(delta) FROM (
			SELECT toDate(timestamp) AS d,
			       download_count - lagInFrame(download_count) OVER w AS delta,
			       lagInFrame(timestamp) OVER w AS prev_ts
			FROM github_download_stats
			WHERE timestamp >= today() - ?
			WINDOW w AS (PARTITION BY release_tag, asset_name ORDER BY timestamp)
		)
		WHERE prev_ts > toDateTime(0) AND delta > 0
		GROUP BY d
	`
	drows, err := s.conn.Query(ctx, perDay, days)
	if err != nil {
		return err
	}
	defer drows.Close()
	for drows.Next() {
		var d time.Time
		// Signed: delta is a difference of two counters, so ClickHouse widens it
		// to Int64 even though the WHERE clause has already dropped the negative
		// ones.
		var n int64
		if err := drows.Scan(&d, &n); err != nil {
			return err
		}
		if i, found := idx[d.Format("2006-01-02")]; found && n > 0 {
			snap.GhDaily[i] = uint64(n)
		}
	}
	return drows.Err()
}

// collectCurves builds one cumulative curve per release, aligned on days since
// that release's first sample, so two releases can be compared at the same age
// instead of at the same date.
//
// Deliberately not windowed: a curve needs the release's whole life, and for a
// superseded tag that life ended before any window this page shows.
func (s *dbSrv) collectCurves(ctx context.Context, snap *releaseStats, _ map[string]int, _ int) error {
	const query = `
		SELECT release_tag, d, sum(cnt) AS total
		FROM (
			SELECT release_tag, asset_name, toDate(timestamp) AS d,
			       max(download_count) AS cnt
			FROM github_download_stats
			GROUP BY release_tag, asset_name, d
		)
		GROUP BY release_tag, d
		ORDER BY release_tag, d
	`
	rows, err := s.conn.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	type raw struct {
		first time.Time
		pts   map[int]uint64
		last  int
	}
	byTag := map[string]*raw{}
	for rows.Next() {
		var tag string
		var d time.Time
		var total uint64
		if err := rows.Scan(&tag, &d, &total); err != nil {
			return err
		}
		r := byTag[tag]
		if r == nil {
			r = &raw{first: d, pts: map[int]uint64{}}
			byTag[tag] = r
		}
		// ORDER BY guarantees the first row for a tag is its earliest day.
		// Rounded, not truncated: a 23-hour day would otherwise fold two dates
		// onto one index and silently lose a point.
		i := int(math.Round(d.Sub(r.first).Hours() / 24))
		r.pts[i] = total
		if i > r.last {
			r.last = i
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	tags := make([]string, 0, len(byTag))
	for tag := range byTag {
		tags = append(tags, tag)
	}
	// Chronological, so a new release appends a colour instead of renumbering the
	// ones already on the chart.
	sort.Slice(tags, func(i, j int) bool { return byTag[tags[i]].first.Before(byTag[tags[j]].first) })
	if len(tags) > maxCurves {
		tags = tags[len(tags)-maxCurves:]
	}

	for _, tag := range tags {
		r := byTag[tag]
		// A tag sampled once - polling started after it was already superseded -
		// has no curve, only a point. Drawing it would put a dot on the axis that
		// reads as "nobody downloaded it".
		if r.last == 0 {
			continue
		}

		baseline := r.pts[0]
		series := make([]float64, r.last+1)
		var running uint64
		for i := 0; i <= r.last; i++ {
			if v, ok := r.pts[i]; ok && v > running {
				running = v // a cumulative counter cannot go down; a gap in
			} //             polling carries the previous value forward rather
			//               than dropping the curve to zero for that day.
			if running < baseline {
				running = baseline
			}
			series[i] = float64(running - baseline)
		}

		c := releaseCurve{Tag: tag, Total: uint64(series[len(series)-1])}
		if r.last >= curveHorizon {
			c.AtHorizon, c.Reached = uint64(series[curveHorizon]), true
		} else {
			c.AtHorizon = c.Total
		}
		// Clipped to the horizon for drawing. Left unclipped, the release that
		// stayed `latest` longest sets the X scale and squeezes everyone's first
		// weeks into the left edge - reintroducing the tenure bias the whole
		// alignment exists to remove. Total is taken above, from the full series.
		if len(series) > curveHorizon+1 {
			series = series[:curveHorizon+1]
		}
		c.Series = series
		snap.Curves = append(snap.Curves, c)
		if len(series) > snap.CurveDays {
			snap.CurveDays = len(series)
		}
	}
	return nil
}

func mapToCounts(m map[string]uint64) []labelCount {
	out := make([]labelCount, 0, len(m))
	for k, v := range m {
		out = append(out, labelCount{k, v})
	}
	return out
}

// topN sorts descending and folds the tail into "other" rather than dropping it,
// so the bars still add up to the total shown above them. An existing "other"
// bucket from classifyAsset merges into the same row.
func topN(items []labelCount, n int) []labelCount {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].N != items[j].N {
			return items[i].N > items[j].N
		}
		return items[i].Label < items[j].Label
	})
	if len(items) <= n {
		return items
	}
	var rest uint64
	for _, it := range items[n:] {
		rest += it.N
	}
	head := slices.Clone(items[:n])
	for i := range head {
		if head[i].Label == unknownBucket {
			head[i].N += rest
			return head
		}
	}
	return append(head, labelCount{unknownBucket, rest})
}

// ---------------------------------------------------------------- worker

type statsCollector struct {
	src   statsSource
	views []int
	pages atomic.Pointer[map[int]*statsPage]
}

type statsPage struct {
	HTML        []byte
	GeneratedAt time.Time
	ETag        string
}

var statsViewLengths = []int{7, 30, 180, 360}

func NewStatsCollector(src statsSource, days int) *statsCollector {
	views := []int{}
	for _, v := range append(append([]int{}, statsViewLengths...), days) {
		if v > 0 && v <= days && !slices.Contains(views, v) {
			views = append(views, v)
		}
	}
	slices.Sort(views)
	return &statsCollector{src: src, views: views}
}

func (sc *statsCollector) defaultView() int {
	if len(sc.views) == 0 {
		return 0
	}
	return sc.views[len(sc.views)-1]
}

func (sc *statsCollector) Worker(ctx context.Context, every time.Duration) {
	sc.refresh(ctx)

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sc.refresh(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (sc *statsCollector) refresh(ctx context.Context) {
	pages := make(map[int]*statsPage, len(sc.views))
	var totalBytes int
	for _, v := range sc.views {
		snap, err := sc.src.CollectStats(ctx, v)
		if err != nil {
			// Keep whatever is already published. A failed refresh should serve
			// stale numbers, not an empty page.
			log.WithError(err).WithField("days", v).Error("stats: collection failed")
			return
		}
		snap.Views = sc.views
		body := renderStats(snap)
		totalBytes += len(body)
		pages[v] = &statsPage{
			HTML:        body,
			GeneratedAt: snap.GeneratedAt,
			ETag:        fmt.Sprintf(`W/"%d-%d-%d"`, snap.GeneratedAt.Unix(), v, len(body)),
		}
	}
	sc.pages.Store(&pages)
	log.WithField("views", len(pages)).WithField("bytes", totalBytes).Info("stats: pages rebuilt")
}

// viewFromRequest reads ?days=. Only values from the view list are honoured -
// any other number would force a query on demand, which the handler must never do.
func (sc *statsCollector) viewFromRequest(r *http.Request, pages map[int]*statsPage) int {
	if raw := r.URL.Query().Get("days"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			if _, ok := pages[v]; ok {
				return v
			}
		}
	}
	return sc.defaultView()
}

func (sc *statsCollector) Handler(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	if !hostMatches(r.Host) {
		http.NotFound(w, r)
		return
	}
	ptr := sc.pages.Load()
	var page *statsPage
	if ptr != nil {
		pages := *ptr
		page = pages[sc.viewFromRequest(r, pages)]
	}
	if page == nil {
		// The first collection is still running. A 503 with Retry-After is more
		// honest than an empty page a client or CDN might cache.
		w.Header().Set("retry-after", "30")
		http.Error(w, "stats not generated yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("content-security-policy", statsCSP)
	w.Header().Set("x-content-type-options", "nosniff")
	w.Header().Set("referrer-policy", "no-referrer")
	w.Header().Set("etag", page.ETag)
	w.Header().Set("last-modified", page.GeneratedAt.UTC().Format(http.TimeFormat))
	w.Header().Set("cache-control", "public, max-age=300")
	if r.Header.Get("if-none-match") == page.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page.HTML)
}
