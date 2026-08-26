package main

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/julienschmidt/httprouter"
)

// Endpoints shown on /stats, and the only three this service logs - the router
// registers AccessLogMiddleware under exactly these names.
//
// Every panel filters on this list rather than excluding what it does not want.
// The clients panel used to say endpoint_name != 'releases' instead, from when
// the release redirect lived here; those rows moved to releases_access_log and
// were deleted from access_log by cmd/releases/MIGRATION.sql, so the exclusion
// had stopped matching anything at all. A negative filter is the wrong shape
// here anyway: a new endpoint added to the router would silently enter the
// client counts while being absent from every chart beside them.
var statsEndpoints = []string{"debuginfo", "executable", "source"}

// unresolvedLabel stands in for an empty resolved_host. An empty field means the
// request never reached any upstream (unknown build ID, or refused by the
// backoff) - that is ~94% of traffic, so it must be visible, not filtered out.
const unresolvedLabel = "unresolved"

type statusCounts struct {
	Served  uint64 // 200, transfer completed
	Aborted uint64 // 200, but the client disconnected mid-transfer
	// NotFound means "we do not have it", not a failure: 404, 501 (the host does
	// not serve sources) and 503. The 503 comes from ErrDebuginfodTemporaryDown,
	// i.e. from our own Down flag in the servers map - a configuration decision,
	// not an upstream fault, so counting it as 5xx would overstate breakage.
	// Old rows with no recorded status live here too, which is what they were.
	NotFound uint64
	Err5xx   uint64 // genuine server errors: 5xx excluding 501 and 503
}

func (c statusCounts) total() uint64 { return c.Served + c.Aborted + c.NotFound + c.Err5xx }

type probeDay struct {
	OK, Fail         uint64
	P50, P95         uint32 // probes that succeeded
	FailP50, FailP95 uint32 // probes that failed
}

type thruDay struct {
	N        uint64
	P50, P90 float64 // MiB/s
}

type statsSnapshot struct {
	GeneratedAt time.Time
	Took        time.Duration
	Days        []string
	// Smooth is the moving-average window chosen for the view length; Views is the
	// list of available lengths, from which the renderer draws the switcher.
	Smooth int
	Views  []int

	// Traffic: host -> endpoint -> one value per day in Days.
	Traffic map[string]map[string][]statusCounts
	Hosts   []string // sorted by volume descending, unresolved always first

	// Unique clients and build IDs per day - without these a request count could
	// be load from many clients or one client in a loop, and you cannot tell.
	Clients  []uint64
	BuildIDs []uint64
	Requests []uint64

	// Bytes over the 24h / 7d / 30d / full-range windows.
	Bytes      map[string][4]uint64
	BytesTotal [4]uint64

	Probes     map[string][]probeDay
	ProbeHosts []string
	ProbeTotal uint64
	ProbeOK    uint64

	// Probes in the last 24 hours, per host, plus the total across all of them.
	//
	// tryAllServers probes every configured upstream on every cold build ID, so
	// a host that is still in the map cannot go a day without rows. Zero here
	// therefore means it is no longer being asked at all - it was removed from
	// the servers map (elfutils and alpine are commented out there) - and the
	// page says so beside its name.
	//
	// The total is what makes that reading safe. On a day with no cold build
	// IDs nobody is probed, every host reads zero, and marking them all offline
	// would be reporting our own idleness as their outage. See offlineHosts.
	//
	// Not sliced with the views: "last 24 h" is the same window whichever chart
	// length is on screen.
	ProbeRecent      map[string]uint64
	ProbeRecentTotal uint64

	Thru      map[string][]thruDay
	ThruHosts []string
	ThruMax   float64

	// Where the traffic comes from: country -> one value per day in Days.
	//
	// Daily rather than one total per country, because /stats slices four views
	// out of a single collection and a total cannot be cut down to seven days.
	// Requests sum across days correctly; distinct clients do NOT, which is why
	// CountryClients holds the per-day figure and the panel reports its peak
	// rather than a sum - a client active on thirty days would otherwise count
	// as thirty clients.
	Country        map[string][]uint64
	CountryClients map[string][]uint64
	Countries      []string // sorted by request volume descending

	// Cache and partition usage: the last measurement of each day, in MiB.
	// HasCacheStats stays false while the table is empty - the whole section then
	// disappears from the page instead of showing flat zeros.
	HasCacheStats bool
	CacheBytes    []float64
	CacheTmp      []float64
	CacheEntries  []float64
	FsFree        []float64
	CacheLast     cacheUsage
	CacheMaxBytes uint64
}

// statsSource exists so the render tests need no ClickHouse - the same reason as
// accessLogger in context.go and stateStore in finder.go.
// *dbSrv satisfies it implicitly.
type statsSource interface {
	CollectStats(ctx context.Context, days int) (*statsSnapshot, error)
}

// statsQueryTimeout is deliberately far above the 5s used for point queries:
// these aggregates scan the whole access_log range (millions of rows). They run
// in the background, so nobody is waiting on them.
const statsQueryTimeout = 60 * time.Second

func (s *dbSrv) CollectStats(ctx context.Context, days int) (*statsSnapshot, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, statsQueryTimeout)
	defer cancel()

	snap := &statsSnapshot{
		GeneratedAt: time.Now(),
		Traffic:     map[string]map[string][]statusCounts{},
		Bytes:       map[string][4]uint64{},
		Probes:      map[string][]probeDay{},
		ProbeRecent: map[string]uint64{},
		Thru:        map[string][]thruDay{},

		Country:        map[string][]uint64{},
		CountryClients: map[string][]uint64{},
	}

	// The time axis is built from the clock, not from the data: a day with no
	// traffic must stay a gap in the chart rather than vanish and pull the
	// neighbouring points together.
	end := time.Now().UTC().Truncate(24 * time.Hour)
	idx := make(map[string]int, days+1)
	for i := days; i >= 0; i-- {
		d := end.AddDate(0, 0, -i).Format("2006-01-02")
		idx[d] = len(snap.Days)
		snap.Days = append(snap.Days, d)
	}
	n := len(snap.Days)
	snap.Smooth = smoothWindowFor(days)

	if err := s.collectTraffic(ctx, snap, idx, n, days); err != nil {
		return nil, fmt.Errorf("traffic: %w", err)
	}
	if err := s.collectClients(ctx, snap, idx, n, days); err != nil {
		return nil, fmt.Errorf("clients: %w", err)
	}
	if err := s.collectBytes(ctx, snap, days); err != nil {
		return nil, fmt.Errorf("bytes: %w", err)
	}
	if err := s.collectProbes(ctx, snap, idx, n, days); err != nil {
		return nil, fmt.Errorf("probes: %w", err)
	}
	if err := s.collectThroughput(ctx, snap, idx, n, days); err != nil {
		return nil, fmt.Errorf("throughput: %w", err)
	}
	if err := s.collectCountries(ctx, snap, idx, n, days); err != nil {
		return nil, fmt.Errorf("countries: %w", err)
	}
	if err := s.collectProbeRecent(ctx, snap); err != nil {
		return nil, fmt.Errorf("recent probes: %w", err)
	}
	if err := s.collectCacheUsage(ctx, snap, idx, n, days); err != nil {
		return nil, fmt.Errorf("cache usage: %w", err)
	}

	snap.Took = time.Since(started)
	return snap, nil
}

// endpointInList builds the IN list directly into the query text. statsEndpoints
// is a constant in the code, not user input, so there is nothing to inject here -
// while binding a slice to IN (?) behaves differently between driver versions and
// cannot be verified without a live database.
func endpointInList() string {
	quoted := make([]string, len(statsEndpoints))
	for i, ep := range statsEndpoints {
		quoted[i] = "'" + ep + "'"
	}
	return strings.Join(quoted, ", ")
}

// excludeCI drops requests made by GitHub Actions runners.
//
// /stats is meant to describe people using the service. A workflow that
// installs pwndbg on every push makes a handful of build IDs look like
// sustained demand and inflates the distinct-client count.
//
// One mechanism now: the row's own tags, written when it was logged and decided
// against the range list as it stood at that moment. There used to be a second
// - an ip_trie dictionary in ClickHouse, consulted for rows that predated
// tagging - and it was removed once scripts/backfill_tags.py had classified
// every one of them. Do not bring it back as a general safety net: matching an
// old row against today's ranges reattributes traffic every time GitHub hands a
// prefix back to Azure, which is precisely why the decision is recorded per row
// instead of recomputed.
//
// Rows tagged "unclassified" - logged before the range list had ever loaded -
// are counted here rather than dropped. That is the conservative direction:
// they show up as ordinary traffic instead of silently vanishing, and
// scripts/backfill_tags.py exists to resolve them.
const excludeCI = ` AND NOT has(tags, 'github_actions')`

// trafficQuery is a package-level variable rather than a constant inside the
// method so a test can pin the category split - in particular which side 503
// falls on.
var trafficQuery = `
		SELECT toDate(timestamp) AS d,
		       if(resolved_host = '', ?, resolved_host) AS host,
		       endpoint_name AS ep,
		       countIf(status = 200 AND error_msg = '')                          AS served,
		       countIf(status = 200 AND error_msg != '')                         AS aborted,
		       countIf(status != 200 AND (status < 500 OR status IN (501, 503)))  AS notfound,
		       countIf(status >= 500 AND status NOT IN (501, 503))                AS err5xx
		FROM access_log
		WHERE timestamp >= today() - ? AND endpoint_name IN (` + endpointInList() + `)` + excludeCI + `
		GROUP BY d, host, ep
	`

func (s *dbSrv) collectTraffic(ctx context.Context, snap *statsSnapshot, idx map[string]int, n, days int) error {
	rows, err := s.conn.Query(ctx, trafficQuery, unresolvedLabel, days)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			d                                 time.Time
			host, ep                          string
			served, aborted, notfound, err5xx uint64
		)
		if err := rows.Scan(&d, &host, &ep, &served, &aborted, &notfound, &err5xx); err != nil {
			return err
		}
		i, ok := idx[d.Format("2006-01-02")]
		if !ok {
			continue
		}
		byEp := snap.Traffic[host]
		if byEp == nil {
			byEp = map[string][]statusCounts{}
			snap.Traffic[host] = byEp
		}
		if byEp[ep] == nil {
			byEp[ep] = make([]statusCounts, n)
		}
		byEp[ep][i] = statusCounts{served, aborted, notfound, err5xx}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for host := range snap.Traffic {
		snap.Hosts = append(snap.Hosts, host)
	}
	sortHostsByVolume(snap.Hosts, func(h string) uint64 {
		var sum uint64
		for _, series := range snap.Traffic[h] {
			for _, c := range series {
				sum += c.total()
			}
		}
		return sum
	})
	return nil
}

// clientsQuery is a package-level variable for the same two reasons as
// trafficQuery: endpointInList() is a call, so it cannot be a constant, and a
// test can then check it counts the same endpoints the traffic panel does.
var clientsQuery = `
		SELECT toDate(timestamp) AS d, uniq(remote_ip), uniq(buildid), count()
		FROM access_log
		WHERE timestamp >= today() - ? AND endpoint_name IN (` + endpointInList() + `)` + excludeCI + `
		GROUP BY d
	`

func (s *dbSrv) collectClients(ctx context.Context, snap *statsSnapshot, idx map[string]int, n, days int) error {
	rows, err := s.conn.Query(ctx, clientsQuery, days)
	if err != nil {
		return err
	}
	defer rows.Close()

	snap.Clients = make([]uint64, n)
	snap.BuildIDs = make([]uint64, n)
	snap.Requests = make([]uint64, n)
	for rows.Next() {
		var (
			d                   time.Time
			ips, bids, requests uint64
		)
		if err := rows.Scan(&d, &ips, &bids, &requests); err != nil {
			return err
		}
		if i, ok := idx[d.Format("2006-01-02")]; ok {
			snap.Clients[i], snap.BuildIDs[i], snap.Requests[i] = ips, bids, requests
		}
	}
	return rows.Err()
}

func (s *dbSrv) collectBytes(ctx context.Context, snap *statsSnapshot, days int) error {
	// The windows are anchored on the last row in the table, not on now(): the most
	// recent request may be hours old, and then "last 24h" measured from now loses
	// half a day.
	const query = `
		WITH (SELECT max(timestamp) FROM access_log) AS t
		SELECT if(resolved_host = '', ?, resolved_host) AS host,
		       sumIf(bytes_sent, timestamp >= t - INTERVAL 1 DAY)  AS b1,
		       sumIf(bytes_sent, timestamp >= t - INTERVAL 7 DAY)  AS b7,
		       sumIf(bytes_sent, timestamp >= t - INTERVAL 30 DAY) AS b30,
		       sum(bytes_sent)                                     AS ball
		FROM access_log
		WHERE timestamp >= today() - ?` + excludeCI + `
		GROUP BY host
	`
	rows, err := s.conn.Query(ctx, query, unresolvedLabel, days)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			host              string
			b1, b7, b30, ball uint64
		)
		if err := rows.Scan(&host, &b1, &b7, &b30, &ball); err != nil {
			return err
		}
		snap.Bytes[host] = [4]uint64{b1, b7, b30, ball}
		for k, v := range [4]uint64{b1, b7, b30, ball} {
			snap.BytesTotal[k] += v
		}
	}
	return rows.Err()
}

func (s *dbSrv) collectProbes(ctx context.Context, snap *statsSnapshot, idx map[string]int, n, days int) error {
	// quantileIf over an empty set returns nan, which will not Scan into uint32 -
	// hence the explicit zero when that probe category had no rows that day.
	const query = `
		SELECT toDate(timestamp) AS d, resolved_host AS host,
		       countIf(success) AS ok, countIf(NOT success) AS fail,
		       if(countIf(success) > 0,     toUInt32(round(quantileIf(0.5)(duration_ms, success))), 0)      AS p50,
		       if(countIf(success) > 0,     toUInt32(round(quantileIf(0.95)(duration_ms, success))), 0)     AS p95,
		       if(countIf(NOT success) > 0, toUInt32(round(quantileIf(0.5)(duration_ms, NOT success))), 0)  AS f50,
		       if(countIf(NOT success) > 0, toUInt32(round(quantileIf(0.95)(duration_ms, NOT success))), 0) AS f95
		FROM resolve_logs
		WHERE timestamp >= today() - ?
		GROUP BY d, host
	`
	rows, err := s.conn.Query(ctx, query, days)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			d                  time.Time
			host               string
			ok, fail           uint64
			p50, p95, f50, f95 uint32
		)
		if err := rows.Scan(&d, &host, &ok, &fail, &p50, &p95, &f50, &f95); err != nil {
			return err
		}
		i, found := idx[d.Format("2006-01-02")]
		if !found {
			continue
		}
		if snap.Probes[host] == nil {
			snap.Probes[host] = make([]probeDay, n)
		}
		snap.Probes[host][i] = probeDay{ok, fail, p50, p95, f50, f95}
		snap.ProbeTotal += ok + fail
		snap.ProbeOK += ok
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for host := range snap.Probes {
		snap.ProbeHosts = append(snap.ProbeHosts, host)
	}
	sortHostsByVolume(snap.ProbeHosts, func(h string) uint64 {
		var sum uint64
		for _, p := range snap.Probes[h] {
			sum += p.OK + p.Fail
		}
		return sum
	})
	return nil
}

func (s *dbSrv) collectThroughput(ctx context.Context, snap *statsSnapshot, idx map[string]int, n, days int) error {
	// Throughput rather than duration_ms: duration grows with file size, so it
	// cannot be compared between hosts. duration_100kb_ms would be better (fixed
	// sample size) but we only started recording it recently.
	// HIT and COALESCED are excluded because their bytes came from disk, not from
	// an upstream.
	const query = `
		SELECT toDate(timestamp) AS d, resolved_host AS host, count() AS n,
		       quantile(0.5)(bytes_sent / greatest(duration_ms, 1) * 1000 / 1048576) AS p50,
		       quantile(0.9)(bytes_sent / greatest(duration_ms, 1) * 1000 / 1048576) AS p90
		FROM access_log
		WHERE timestamp >= today() - ?
		  AND status = 200 AND error_msg = '' AND bytes_sent > 102400
		  AND cache_status NOT IN ('HIT', 'COALESCED') AND resolved_host != ''` + excludeCI + `
		GROUP BY d, host
	`
	rows, err := s.conn.Query(ctx, query, days)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			d        time.Time
			host     string
			cnt      uint64
			p50, p90 float64
		)
		if err := rows.Scan(&d, &host, &cnt, &p50, &p90); err != nil {
			return err
		}
		i, found := idx[d.Format("2006-01-02")]
		if !found {
			continue
		}
		if snap.Thru[host] == nil {
			snap.Thru[host] = make([]thruDay, n)
		}
		snap.Thru[host][i] = thruDay{cnt, p50, p90}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for host := range snap.Thru {
		snap.ThruHosts = append(snap.ThruHosts, host)
	}
	sortHostsByVolume(snap.ThruHosts, func(h string) uint64 {
		var sum uint64
		for _, t := range snap.Thru[h] {
			sum += t.N
		}
		return sum
	})
	// One shared scale across hosts - the unit is the same, so comparing panels is
	// meaningful here, unlike with request volumes.
	for _, series := range snap.Thru {
		for _, v := range smoothPairs(thruPairs(series), snap.Smooth) {
			if v[1] > snap.ThruMax {
				snap.ThruMax = v[1]
			}
		}
	}
	if snap.ThruMax == 0 {
		snap.ThruMax = 1
	}
	return nil
}

const mib = 1 << 20

// countryQuery is a package-level variable for the same reason as trafficQuery
// and clientsQuery: endpointInList() is a call, and a test can then check that
// this panel counts the same endpoints and excludes the same CI traffic as the
// ones beside it.
//
// country = ” is dropped rather than bucketed as "unknown". After the mmdb
// backfill the only addresses left without one are 127.0.0.1 and the Docker
// bridge - this host talking to itself, which is not a place traffic comes
// from. A visible "unknown" bar would invite reading it as unlocatable users.
var countryQuery = `
		SELECT toDate(timestamp) AS d, country, count(), uniq(remote_ip)
		FROM access_log
		WHERE timestamp >= today() - ? AND endpoint_name IN (` + endpointInList() + `)
		  AND country != ''` + excludeCI + `
		GROUP BY d, country
	`

func (s *dbSrv) collectCountries(ctx context.Context, snap *statsSnapshot, idx map[string]int, n, days int) error {
	rows, err := s.conn.Query(ctx, countryQuery, days)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			day               time.Time
			country           string
			requests, uniques uint64
		)
		if err := rows.Scan(&day, &country, &requests, &uniques); err != nil {
			return err
		}
		i, ok := idx[day.Format("2006-01-02")]
		if !ok {
			continue
		}
		if _, seen := snap.Country[country]; !seen {
			snap.Country[country] = make([]uint64, n)
			snap.CountryClients[country] = make([]uint64, n)
			snap.Countries = append(snap.Countries, country)
		}
		snap.Country[country][i] = requests
		snap.CountryClients[country][i] = uniques
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sortCountries(snap)
	return nil
}

// probeRecentQuery counts the last day only.
//
// A second pass over resolve_logs rather than a countIf folded into the main
// probe query: that one groups by day and scans the whole 360-day window, while
// this one is bounded by a timestamp predicate on the table's own sort key
// (ORDER BY (timestamp, buildid)), so it reads one day of parts instead of all
// of them.
const probeRecentQuery = `
		SELECT resolved_host, count()
		FROM resolve_logs
		WHERE timestamp >= now() - INTERVAL 1 DAY
		GROUP BY resolved_host
	`

func (s *dbSrv) collectProbeRecent(ctx context.Context, snap *statsSnapshot) error {
	rows, err := s.conn.Query(ctx, probeRecentQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			host string
			n    uint64
		)
		if err := rows.Scan(&host, &n); err != nil {
			return err
		}
		snap.ProbeRecent[host] = n
		snap.ProbeRecentTotal += n
	}
	return rows.Err()
}

// isOffline reports whether a host has stopped being probed.
//
// A predicate rather than a precomputed set of hosts, because four panels name
// hosts - traffic, bytes, probes and throughput - each from its own list, and
// the first version marked only one of them. The same upstream then read as
// live in one section and dead in the other. Anything that can name a host can
// ask this.
//
// Two guards, and both are load-bearing:
//
//   - ProbeRecentTotal == 0 means nobody was probed at all. tryAllServers probes
//     every configured upstream on every cold build ID, so that is the signature
//     of a night with no cold build IDs, not of every backend failing at once.
//     Reporting our own idleness as their outage would be worse than saying
//     nothing.
//   - unresolvedLabel is not a host. It stands in for an empty resolved_host -
//     the request never reached an upstream - so it is never probed and would
//     otherwise be marked offline on every page load.
func (s *statsSnapshot) isOffline(host string) bool {
	if s.ProbeRecentTotal == 0 || host == "" || host == unresolvedLabel {
		return false
	}
	return s.ProbeRecent[host] == 0
}

// offlineCount is how many of the probed backends have gone quiet, for the
// sentence that explains the badge.
func (s *statsSnapshot) offlineCount() int {
	n := 0
	for _, host := range s.ProbeHosts {
		if s.isOffline(host) {
			n++
		}
	}
	return n
}

// sortCountries orders by requests over the window, descending. It runs again
// after slicing, because the leader over 360 days need not lead over 7.
func sortCountries(snap *statsSnapshot) {
	sort.Slice(snap.Countries, func(i, j int) bool {
		a, b := sumU(snap.Country[snap.Countries[i]]), sumU(snap.Country[snap.Countries[j]])
		if a != b {
			return a > b
		}
		return snap.Countries[i] < snap.Countries[j]
	})
}

func sumU(v []uint64) uint64 {
	var sum uint64
	for _, x := range v {
		sum += x
	}
	return sum
}

func maxU(v []uint64) uint64 {
	var max uint64
	for _, x := range v {
		if x > max {
			max = x
		}
	}
	return max
}

func (s *dbSrv) collectCacheUsage(ctx context.Context, snap *statsSnapshot, idx map[string]int, n, days int) error {
	// argMax rather than avg: we want the state at the end of the day, because that
	// is what says how much space we actually occupy. An average over a day in which
	// eviction freed half the cache describes no moment that ever existed.
	const query = `
		SELECT toDate(timestamp) AS d,
		       argMax(entries, timestamp)   AS entries,
		       argMax(bytes, timestamp)     AS bytes,
		       argMax(tmp_bytes, timestamp) AS tmp_bytes,
		       argMax(fs_free, timestamp)   AS fs_free
		FROM cache_stats
		WHERE timestamp >= today() - ?
		GROUP BY d
	`
	rows, err := s.conn.Query(ctx, query, days)
	if err != nil {
		return err
	}
	defer rows.Close()

	snap.CacheBytes = make([]float64, n)
	snap.CacheTmp = make([]float64, n)
	snap.CacheEntries = make([]float64, n)
	snap.FsFree = make([]float64, n)
	for rows.Next() {
		var (
			d                                time.Time
			entries, bytes, tmpBytes, fsFree uint64
		)
		if err := rows.Scan(&d, &entries, &bytes, &tmpBytes, &fsFree); err != nil {
			return err
		}
		i, ok := idx[d.Format("2006-01-02")]
		if !ok {
			continue
		}
		snap.HasCacheStats = true
		snap.CacheEntries[i] = float64(entries)
		snap.CacheBytes[i] = float64(bytes) / mib
		snap.CacheTmp[i] = float64(tmpBytes) / mib
		snap.FsFree[i] = float64(fsFree) / mib
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !snap.HasCacheStats {
		return nil
	}

	// The tiles should show the state now, not the end of the last full day.
	last := s.conn.QueryRow(ctx, `
		SELECT timestamp, entries, bytes, apparent_bytes, tmp_bytes, fs_total, fs_free
		FROM cache_stats ORDER BY timestamp DESC LIMIT 1
	`)
	var u cacheUsage
	if err := last.Scan(&u.Timestamp, &u.Entries, &u.Bytes, &u.ApparentBytes,
		&u.TmpBytes, &u.FsTotal, &u.FsFree); err != nil {
		return err
	}
	snap.CacheLast = u
	return nil
}

// sliceSnapshot returns a view of the last `days` days. It queries nothing - the
// daily series already exist, so a shorter window is a tail cut plus a recompute
// of everything derived from them. All views therefore cost the same as one.
//
// More has to be recomputed than is obvious: host ordering (a host busy six months
// ago need not be busy this week), probe totals, and the shared throughput scale.
// Copying only the series would leave headings describing 180 days above charts
// covering seven.
func sliceSnapshot(src *statsSnapshot, days int) *statsSnapshot {
	n := len(src.Days)
	keep := days + 1
	if keep >= n {
		// Full window: nothing to cut, but do NOT return src. First, the caller
		// writes its own fields into the result and would mutate the source the
		// other views are cut from. Second, Smooth has to be set the same way as
		// in the cut views - otherwise the longest chart would be the only one
		// inheriting somebody else's value, or zero.
		out := *src
		out.Smooth = smoothWindowFor(days)
		return &out
	}
	from := n - keep
	cutF := func(v []float64) []float64 {
		if len(v) < n {
			return v
		}
		return v[from:]
	}

	out := *src // kopiujemy pola skalarne, mapy podmieniamy nizej
	out.Days = src.Days[from:]
	out.Smooth = smoothWindowFor(days)

	out.Traffic = make(map[string]map[string][]statusCounts, len(src.Traffic))
	for host, byEp := range src.Traffic {
		cut := make(map[string][]statusCounts, len(byEp))
		for ep, series := range byEp {
			if len(series) >= n {
				cut[ep] = series[from:]
			} else {
				cut[ep] = series
			}
		}
		out.Traffic[host] = cut
	}
	out.Hosts = append([]string(nil), src.Hosts...)
	sortHostsByVolume(out.Hosts, func(h string) uint64 {
		var sum uint64
		for _, series := range out.Traffic[h] {
			for _, c := range series {
				sum += c.total()
			}
		}
		return sum
	})

	out.Clients, out.BuildIDs, out.Requests = cutU(src.Clients, from, n), cutU(src.BuildIDs, from, n), cutU(src.Requests, from, n)

	out.Probes = make(map[string][]probeDay, len(src.Probes))
	out.ProbeTotal, out.ProbeOK = 0, 0
	for host, series := range src.Probes {
		if len(series) >= n {
			series = series[from:]
		}
		out.Probes[host] = series
		for _, p := range series {
			out.ProbeTotal += p.OK + p.Fail
			out.ProbeOK += p.OK
		}
	}
	out.ProbeHosts = append([]string(nil), src.ProbeHosts...)
	sortHostsByVolume(out.ProbeHosts, func(h string) uint64 {
		var sum uint64
		for _, p := range out.Probes[h] {
			sum += p.OK + p.Fail
		}
		return sum
	})

	out.Thru = make(map[string][]thruDay, len(src.Thru))
	out.ThruMax = 0
	for host, series := range src.Thru {
		if len(series) >= n {
			series = series[from:]
		}
		out.Thru[host] = series
		for _, v := range smoothPairs(thruPairs(series), out.Smooth) {
			if v[1] > out.ThruMax {
				out.ThruMax = v[1]
			}
		}
	}
	if out.ThruMax == 0 {
		out.ThruMax = 1
	}
	out.ThruHosts = append([]string(nil), src.ThruHosts...)
	sortHostsByVolume(out.ThruHosts, func(h string) uint64 {
		var sum uint64
		for _, t := range out.Thru[h] {
			sum += t.N
		}
		return sum
	})

	out.Country = make(map[string][]uint64, len(src.Country))
	out.CountryClients = make(map[string][]uint64, len(src.CountryClients))
	for country, series := range src.Country {
		out.Country[country] = cutU(series, from, n)
		out.CountryClients[country] = cutU(src.CountryClients[country], from, n)
	}
	out.Countries = append([]string(nil), src.Countries...)
	sortCountries(&out)

	out.CacheBytes, out.CacheTmp = cutF(src.CacheBytes), cutF(src.CacheTmp)
	out.CacheEntries, out.FsFree = cutF(src.CacheEntries), cutF(src.FsFree)
	return &out
}

func cutU(v []uint64, from, n int) []uint64 {
	if len(v) < n {
		return v
	}
	return v[from:]
}

// sortHostsByVolume sorts by volume descending, but keeps unresolved first
// regardless - it is not an upstream, it is the absence of one.
func sortHostsByVolume(hosts []string, weight func(string) uint64) {
	sort.Slice(hosts, func(i, j int) bool {
		if (hosts[i] == unresolvedLabel) != (hosts[j] == unresolvedLabel) {
			return hosts[i] == unresolvedLabel
		}
		wi, wj := weight(hosts[i]), weight(hosts[j])
		if wi != wj {
			return wi > wj
		}
		return hosts[i] < hosts[j]
	})
}

// ---------------------------------------------------------------- worker

type statsCollector struct {
	src   statsSource
	days  int
	views []int // ascending; the last one is the default

	// Finished pages held whole in memory, one per view length. The handler never
	// touches the database: /stats must answer immediately and must not tie up
	// ClickHouse connections when something indexes it or refreshes it in a loop.
	// Every view comes from ONE set of queries - shorter windows are a tail cut of
	// the same daily series.
	pages atomic.Pointer[map[int]*statsPage]
}

type statsPage struct {
	HTML        []byte
	GeneratedAt time.Time
	ETag        string
}

// statsViewLengths are the windows offered in the switcher, filtered to those
// that fit inside STATS_DAYS - there is no point showing a tab for data we never
// collected. STATS_DAYS itself is always appended last, so the full collected
// range is reachable even for an unusual value.
var statsViewLengths = []int{7, 30, 180, 360}

func NewStatsCollector(src statsSource, days int) *statsCollector {
	views := []int{}
	for _, v := range append(append([]int{}, statsViewLengths...), days) {
		if v > 0 && v <= days && !slices.Contains(views, v) {
			views = append(views, v)
		}
	}
	slices.Sort(views)
	return &statsCollector{src: src, days: days, views: views}
}

// defaultView is the longest window - the others are cut from it.
func (sc *statsCollector) defaultView() int {
	if len(sc.views) == 0 {
		return sc.days
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
	snap, err := sc.src.CollectStats(ctx, sc.days)
	if err != nil {
		// Keep the previous page. A failed refresh should yield stale data, not an
		// empty /stats.
		log.WithError(err).Error("stats: collection failed")
		return
	}
	if Config.CacheMaxBytes > 0 {
		snap.CacheMaxBytes = uint64(Config.CacheMaxBytes)
	}
	snap.Views = sc.views

	pages := make(map[int]*statsPage, len(sc.views))
	var totalBytes int
	for _, v := range sc.views {
		view := sliceSnapshot(snap, v)
		view.Views = sc.views
		body := renderStats(view)
		totalBytes += len(body)
		pages[v] = &statsPage{
			HTML:        body,
			GeneratedAt: snap.GeneratedAt,
			ETag:        fmt.Sprintf(`W/"%d-%d-%d"`, snap.GeneratedAt.Unix(), v, len(body)),
		}
	}
	sc.pages.Store(&pages)
	log.WithField("took", snap.Took.Round(time.Millisecond)).
		WithField("views", len(pages)).
		WithField("bytes", totalBytes).Info("stats: pages rebuilt")
}

// viewFromRequest reads ?days=. Only values from the view list are honoured -
// otherwise any number in the URL would either multiply the rendering work or
// force a query on demand, which the handler must never do.
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
	w.Header().Set("etag", page.ETag)
	w.Header().Set("last-modified", page.GeneratedAt.UTC().Format(http.TimeFormat))
	w.Header().Set("cache-control", "public, max-age=300")
	if match := r.Header.Get("if-none-match"); match == page.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page.HTML)
}

// ---------------------------------------------------------------- helpers

// smoothPairs computes a moving average. Without it a single spike on one day
// sets the Y scale of the whole panel and flattens the rest into a straight line.
//
// The window is a parameter rather than a constant because it has to scale with
// the view length: a 7-day average on a 7-day chart would leave one flat line and
// erase exactly the variation such a view is for.
func smoothWindowFor(days int) int {
	switch {
	case days <= 14:
		return 1 // raw daily values
	case days <= 60:
		return 3
	default:
		return 7
	}
}

func smoothPairs(vals [][2]float64, win int) [][2]float64 {
	if win < 1 {
		win = 1
	}
	out := make([][2]float64, len(vals))
	for i := range vals {
		lo := i - win + 1
		if lo < 0 {
			lo = 0
		}
		var a, b float64
		for _, v := range vals[lo : i+1] {
			a += v[0]
			b += v[1]
		}
		cnt := float64(i - lo + 1)
		out[i] = [2]float64{a / cnt, b / cnt}
	}
	return out
}

func smoothOne(vals []float64, win int) []float64 {
	if win < 1 {
		win = 1
	}
	out := make([]float64, len(vals))
	for i := range vals {
		lo := i - win + 1
		if lo < 0 {
			lo = 0
		}
		var a float64
		for _, v := range vals[lo : i+1] {
			a += v
		}
		out[i] = a / float64(i-lo+1)
	}
	return out
}

func thruPairs(series []thruDay) [][2]float64 {
	out := make([][2]float64, len(series))
	for i, t := range series {
		out[i] = [2]float64{t.P50, t.P90}
	}
	return out
}

func fmtCount(n uint64) string {
	switch {
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	case n >= 1_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000)) + "k"
	default:
		return strconv.FormatUint(n, 10)
	}
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

func fmtBytes(n uint64) string {
	v := float64(n)
	for _, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if v < 1024 {
			if unit == "B" {
				return fmt.Sprintf("%.0f B", v)
			}
			return fmt.Sprintf("%.1f %s", v, unit)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.1f PiB", v)
}

func pct(part, whole uint64) string {
	if whole == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(part)/float64(whole))
}

func esc(s string) string { return html.EscapeString(s) }
