package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStatsSource lets the handler and the renderer be tested without ClickHouse -
// that is what the statsSource interface is for.
type fakeStatsSource struct {
	calls atomic.Int32
	snap  *statsSnapshot
	err   error
}

func (f *fakeStatsSource) CollectStats(context.Context, int) (*statsSnapshot, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

func sampleSnapshot(days int) *statsSnapshot {
	s := &statsSnapshot{
		GeneratedAt: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		Took:        123 * time.Millisecond,
		Traffic:     map[string]map[string][]statusCounts{},
		Bytes:       map[string][4]uint64{},
		Probes:      map[string][]probeDay{},
		Thru:        map[string][]thruDay{},

		Country:        map[string][]uint64{},
		CountryClients: map[string][]uint64{},
	}
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < days; i++ {
		s.Days = append(s.Days, base.AddDate(0, 0, i).Format("2006-01-02"))
	}
	n := len(s.Days)

	// elfutils appears in every host-keyed panel, because the badge has to reach
	// all four of them and a fixture that only covers the probe panel is how the
	// page came to contradict itself in the first place.
	for _, host := range []string{unresolvedLabel, "fedora", "elfutils"} {
		s.Traffic[host] = map[string][]statusCounts{}
		for _, ep := range statsEndpoints {
			series := make([]statusCounts, n)
			for i := range series {
				if host == unresolvedLabel {
					series[i] = statusCounts{NotFound: uint64(100 + i)}
					continue
				}
				series[i] = statusCounts{Served: uint64(50 + i), NotFound: 2}
				if i%20 == 0 {
					series[i].Aborted = 1
				}
				if i%50 == 0 {
					series[i].Err5xx = 1
				}
			}
			s.Traffic[host][ep] = series
		}
		s.Hosts = append(s.Hosts, host)
		s.Bytes[host] = [4]uint64{1 << 20, 8 << 20, 32 << 20, 512 << 20}
		for k := range s.BytesTotal {
			s.BytesTotal[k] += s.Bytes[host][k]
		}
	}

	// Weighted so the order is unambiguous and so the tail is long enough to be
	// folded: countryRows is 12.
	for k, code := range []string{"US", "PL", "DE", "SG", "BR", "CN", "IT", "FR", "JP", "GB",
		"NL", "SE", "ES", "CA", "AU", "XX"} {
		req := make([]uint64, n)
		cli := make([]uint64, n)
		for i := range n {
			req[i] = uint64((len(code)*0 + 100 - 5*k) + i)
			cli[i] = uint64(20 - k)
		}
		s.Country[code] = req
		s.CountryClients[code] = cli
		s.Countries = append(s.Countries, code)
	}
	sortCountries(s)

	s.Clients = make([]uint64, n)
	s.BuildIDs = make([]uint64, n)
	s.Requests = make([]uint64, n)
	probes := make([]probeDay, n)
	thru := make([]thruDay, n)
	for i := 0; i < n; i++ {
		s.Clients[i] = uint64(200 + i)
		s.BuildIDs[i] = uint64(400 + i)
		s.Requests[i] = uint64(10000 + 10*i)
		probes[i] = probeDay{OK: uint64(i % 5), Fail: 400, P50: 70, P95: 480, FailP50: 35, FailP95: 120}
		thru[i] = thruDay{N: 100, P50: 1.5, P90: 4.25}
	}
	s.Probes["fedora"] = probes
	s.ProbeHosts = []string{"fedora", "elfutils"}
	s.Probes["elfutils"] = s.Probes["fedora"]
	// fedora is still being probed; elfutils has history but was dropped from the
	// servers map, which is exactly the pair the offline badge exists to tell
	// apart. A non-zero total is what makes the zero meaningful.
	s.ProbeRecent = map[string]uint64{"fedora": 4200}
	s.ProbeRecentTotal = 4200
	for _, p := range probes {
		s.ProbeTotal += p.OK + p.Fail
		s.ProbeOK += p.OK
	}
	s.Thru["fedora"] = thru
	s.Thru["elfutils"] = thru
	s.ThruHosts = []string{"fedora", "elfutils"}
	s.ThruMax = 4.25
	return s
}

func TestStatsHandlerUnavailableBeforeFirstRefresh(t *testing.T) {
	sc := NewStatsCollector(&fakeStatsSource{snap: sampleSnapshot(30)}, 30)

	rec := httptest.NewRecorder()
	sc.Handler(rec, httptest.NewRequest(http.MethodGet, "/stats", nil), nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("przed pierwszym zebraniem oczekiwano 503, jest %d", rec.Code)
	}
	// Without Retry-After a client or CDN may treat the 503 as permanent.
	if rec.Header().Get("retry-after") == "" {
		t.Error("brak naglowka retry-after")
	}
}

func TestStatsHandlerServesRenderedPage(t *testing.T) {
	src := &fakeStatsSource{snap: sampleSnapshot(60)}
	sc := NewStatsCollector(src, 60)
	sc.refresh(context.Background())

	rec := httptest.NewRecorder()
	sc.Handler(rec, httptest.NewRequest(http.MethodGet, "/stats", nil), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, oczekiwano 200", rec.Code)
	}
	if ct := rec.Header().Get("content-type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Who is making these requests",
		"Traffic by upstream and endpoint",
		"Bytes served",
		"Resolution probes",
		"Upstream throughput",
		"<svg", "</html>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("w stronie brakuje %q", want)
		}
	}
	// Every panel carries its own tooltip data; without it the JS trips over null.
	if strings.Count(body, "data-series=") != strings.Count(body, "data-labels=") {
		t.Error("data-series i data-labels nie sa w parze")
	}
}

func TestStatsHandlerHonoursETag(t *testing.T) {
	sc := NewStatsCollector(&fakeStatsSource{snap: sampleSnapshot(20)}, 20)
	sc.refresh(context.Background())

	first := httptest.NewRecorder()
	sc.Handler(first, httptest.NewRequest(http.MethodGet, "/stats", nil), nil)
	etag := first.Header().Get("etag")
	if etag == "" {
		t.Fatal("brak etag")
	}

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.Header.Set("if-none-match", etag)
	second := httptest.NewRecorder()
	sc.Handler(second, req, nil)

	if second.Code != http.StatusNotModified {
		t.Fatalf("przy pasujacym etagu oczekiwano 304, jest %d", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Error("304 nie moze miec ciala")
	}
}

// A failed refresh must leave the previous page in place. Otherwise a momentary
// ClickHouse outage would turn a working /stats into a 503 until the next cycle.
func TestStatsRefreshFailureKeepsPreviousPage(t *testing.T) {
	src := &fakeStatsSource{snap: sampleSnapshot(15)}
	sc := NewStatsCollector(src, 15)
	sc.refresh(context.Background())

	before := sc.pages.Load()
	if before == nil {
		t.Fatal("pierwsze zebranie nie zapisalo stron")
	}
	beforeETag := (*before)[sc.defaultView()].ETag

	src.err = errors.New("clickhouse padl")
	sc.refresh(context.Background())

	after := sc.pages.Load()
	if after == nil {
		t.Fatal("nieudane odswiezenie skasowalo strony")
	}
	if got := (*after)[sc.defaultView()].ETag; got != beforeETag {
		t.Error("strona zostala podmieniona mimo bledu")
	}
}

// A panel with no successful probe at all must not draw a band on the axis - that
// would read as a measured zero, when it is an absence of data.
func TestBandPanelSkipsEmptySeries(t *testing.T) {
	empty := make([][2]float64, 30)
	if svg := bandPanel(empty, 1, "x", 0, 7); strings.Contains(svg, "polygon") {
		t.Error("dla pustej serii nie powinno byc wielokata")
	}

	filled := make([][2]float64, 30)
	for i := range filled {
		filled[i] = [2]float64{10, 20}
	}
	if svg := bandPanel(filled, 20, "x", 0, 7); !strings.Contains(svg, "polygon") {
		t.Error("dla niepustej serii wielokat powinien byc")
	}
}

// The rare categories are a fraction of a per mille of traffic, so they are
// invisible in the stack. The ticks below the axis are the only reason they can be
// found at all.
func TestTrafficPanelMarksRareCategories(t *testing.T) {
	series := make([]statusCounts, 40)
	for i := range series {
		series[i] = statusCounts{Served: 10_000}
	}
	if svg := trafficPanel(series, 10_000, "x", 7); strings.Contains(svg, "<rect") {
		t.Error("bez aborted i 5xx nie powinno byc znacznikow")
	}

	series[7].Aborted = 1
	series[9].Err5xx = 1
	svg := trafficPanel(series, 10_000, "x", 7)
	if n := strings.Count(svg, "<rect"); n != 2 {
		t.Errorf("oczekiwano 2 znacznikow, jest %d", n)
	}
}

// median skips days with no traffic: a zero is an absent measurement, not a
// measurement equal to zero, and counting it would drag the result down the further
// the range extends.
func TestMedianIgnoresZeroDays(t *testing.T) {
	if got := median([]float64{0, 0, 0, 10, 20, 30}); got != 20 {
		t.Errorf("median = %v, oczekiwano 20", got)
	}
	if got := median([]float64{0, 0}); got != 0 {
		t.Errorf("dla samych zer median = %v, oczekiwano 0", got)
	}
}

func TestSeriesJSONMatchesDayCount(t *testing.T) {
	days := []string{"2026-01-01", "2026-01-02", "2026-01-03"}
	got := seriesJSON(days, []float64{1, 2, 3}, []float64{4, 5, 6})
	want := `[["2026-01-01",1,4],["2026-01-02",2,5],["2026-01-03",3,6]]`
	if got != want {
		t.Errorf("seriesJSON = %s, oczekiwano %s", got, want)
	}
}

func TestFormatHelpers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{fmtCount(999), "999"},
		{fmtCount(1500), "1.5k"},
		{fmtCount(2_000_000), "2M"},
		{fmtBytes(512), "512 B"},
		{fmtBytes(1536), "1.5 KiB"},
		{fmtBytes(1 << 30), "1.0 GiB"},
		{pct(1, 4), "25%"},
		{pct(0, 0), "0%"},
	} {
		if tc.in != tc.want {
			t.Errorf("= %q, oczekiwano %q", tc.in, tc.want)
		}
	}
}

// The renderer must not emit an unescaped host name - it reaches the HTML straight
// from a database column.
func TestRenderEscapesHostNames(t *testing.T) {
	snap := sampleSnapshot(10)
	evil := `<script>alert(1)</script>`
	snap.Traffic[evil] = snap.Traffic["fedora"]
	snap.Hosts = append(snap.Hosts, evil)

	body := string(renderStats(snap))
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("nazwa hosta trafila do HTML bez escapowania")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("brak zescapowanej wersji nazwy")
	}
}

func TestWorkerRefreshesOnTick(t *testing.T) {
	src := &fakeStatsSource{snap: sampleSnapshot(10)}
	sc := NewStatsCollector(src, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Worker(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if src.calls.Load() >= 3 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("worker wykonal tylko %d zebran", src.calls.Load())
}

// A 503 comes from ErrDebuginfodTemporaryDown, i.e. from the Down flag in the
// servers map - our own decision to disable an upstream, not a fault on its side.
// It must therefore count as "we do not have it", otherwise the chart reports
// breakage where there is configuration.
func TestTrafficQueryCounts503AsNotFound(t *testing.T) {
	if !strings.Contains(trafficQuery, "status IN (501, 503)") {
		t.Error("503 nie jest liczone do notfound")
	}
	if !strings.Contains(trafficQuery, "status NOT IN (501, 503)") {
		t.Error("503 nie jest wykluczone z err5xx")
	}
	if strings.Contains(trafficQuery, "status != 501)") {
		t.Error("zostal stary warunek liczacy 503 jako blad serwera")
	}
}

// The endpoint list goes into SQL without binding, so it must stay a constant in
// the code - if it ever starts coming from configuration, this test should hurt.
func TestEndpointInListQuotesEveryEndpoint(t *testing.T) {
	got := endpointInList()
	for _, ep := range statsEndpoints {
		if !strings.Contains(got, "'"+ep+"'") {
			t.Errorf("brak %q w %s", ep, got)
		}
	}
	if strings.ContainsAny(got, ";-") {
		t.Errorf("podejrzany znak w liscie endpointow: %s", got)
	}
}

// The budget line must appear only when it fits within the panel's scale.
// Stretching the axis to 5 s for a host that answers in 120 ms would flatten the
// curve to zero and take away the only information the panel carries.
func TestBandPanelDrawsBudgetLineOnlyInRange(t *testing.T) {
	slow := make([][2]float64, 40)
	for i := range slow {
		slow[i] = [2]float64{4000, 9000}
	}
	if svg := bandPanel(slow, 9000, "x", resolutionBudgetMs, 7); !strings.Contains(svg, `class="thr"`) {
		t.Error("przy p95 9000 ms kreska 5 s powinna byc widoczna")
	}

	fast := make([][2]float64, 40)
	for i := range fast {
		fast[i] = [2]float64{40, 120}
	}
	if svg := bandPanel(fast, 120, "x", resolutionBudgetMs, 7); strings.Contains(svg, `class="thr"`) {
		t.Error("przy p95 120 ms kreska 5 s lezalaby poza panelem")
	}

	// Throughput is in MiB/s, so a time threshold makes no sense there.
	if svg := bandPanel(slow, 9000, "x", 0, 7); strings.Contains(svg, `class="thr"`) {
		t.Error("threshold=0 nie powinien rysowac kreski")
	}
}

// The threshold must follow the constant rather than being a written-in number -
// otherwise changing the timeout leaves the line on the chart in the wrong place.
func TestBudgetLineFollowsResolutionTimeout(t *testing.T) {
	if want := float64(maxResolutionTimeout.Milliseconds()); resolutionBudgetMs != want {
		t.Errorf("resolutionBudgetMs = %v, oczekiwano %v", resolutionBudgetMs, want)
	}
}

// The views must come from one set of queries - otherwise every extra window would
// multiply the ClickHouse load on each refresh.
func TestStatsCollectorBuildsAllViewsFromOneCollect(t *testing.T) {
	src := &fakeStatsSource{snap: sampleSnapshot(361)}
	sc := NewStatsCollector(src, 360)
	sc.refresh(context.Background())

	if n := src.calls.Load(); n != 1 {
		t.Errorf("CollectStats wywolane %d razy, oczekiwano 1", n)
	}
	pages := *sc.pages.Load()
	for _, v := range []int{7, 30, 180, 360} {
		if pages[v] == nil {
			t.Errorf("brak widoku %dd", v)
		}
	}
}

// A tab for a window longer than STATS_DAYS would show data that does not exist -
// the chart would end halfway and look like a collection failure.
func TestStatsViewsClampedToCollectedRange(t *testing.T) {
	sc := NewStatsCollector(&fakeStatsSource{snap: sampleSnapshot(31)}, 30)
	for _, v := range sc.views {
		if v > 30 {
			t.Errorf("widok %dd wykracza poza zebrane 30 dni", v)
		}
	}
	if sc.defaultView() != 30 {
		t.Errorf("domyslny widok = %dd, oczekiwano 30", sc.defaultView())
	}
}

// ?days= accepts only values from the view list. Any other number would have to be
// either pre-rendered or queried on demand - and the handler must never touch
// ClickHouse.
func TestStatsHandlerServesRequestedViewAndRejectsOthers(t *testing.T) {
	sc := NewStatsCollector(&fakeStatsSource{snap: sampleSnapshot(361)}, 360)
	sc.refresh(context.Background())

	get := func(q string) string {
		rec := httptest.NewRecorder()
		sc.Handler(rec, httptest.NewRequest(http.MethodGet, "/stats"+q, nil), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q dalo status %d", q, rec.Code)
		}
		return rec.Body.String()
	}

	if !strings.Contains(get("?days=7"), "last 7 days") {
		t.Error("?days=7 nie zwrocil widoku 7-dniowego")
	}
	if !strings.Contains(get("?days=30"), "last 30 days") {
		t.Error("?days=30 nie zwrocil widoku 30-dniowego")
	}
	if !strings.Contains(get("?days=180"), "last 180 days") {
		t.Error("?days=180 nie zwrocil widoku 180-dniowego")
	}
	for _, bad := range []string{"?days=99999", "?days=abc", "?days=-1", "?days="} {
		if !strings.Contains(get(bad), "last 360 days") {
			t.Errorf("%q nie spadl do widoku domyslnego", bad)
		}
	}
}

// Every view needs its own ETag - a shared one would make switching windows return
// 304 and leave the browser showing the previous chart.
func TestStatsViewsHaveDistinctETags(t *testing.T) {
	sc := NewStatsCollector(&fakeStatsSource{snap: sampleSnapshot(361)}, 360)
	sc.refresh(context.Background())

	pages := *sc.pages.Load()
	seen := map[string]int{}
	for days, p := range pages {
		if other, dup := seen[p.ETag]; dup {
			t.Errorf("widoki %dd i %dd maja ten sam ETag %s", days, other, p.ETag)
		}
		seen[p.ETag] = days
	}
}

// A 7-day average on a 7-day chart would leave a single flat line and erase exactly
// the variation such a view is for.
func TestSmoothWindowScalesWithView(t *testing.T) {
	if got := smoothWindowFor(7); got != 1 {
		t.Errorf("dla 7 dni okno = %d, oczekiwano 1 (surowe dane)", got)
	}
	if got := smoothWindowFor(30); got != 3 {
		t.Errorf("dla 30 dni okno = %d, oczekiwano 3", got)
	}
	if got := smoothWindowFor(180); got != 7 {
		t.Errorf("dla 180 dni okno = %d, oczekiwano 7", got)
	}
}

// A shorter view is more than shorter series: the headings, probe totals and host
// ordering must describe that window rather than inherit numbers from 180 days.
func TestSliceSnapshotRecomputesAggregates(t *testing.T) {
	full := sampleSnapshot(181)
	full.Smooth = smoothWindowFor(180)
	cut := sliceSnapshot(full, 7)

	if len(cut.Days) != 8 {
		t.Fatalf("dni w widoku = %d, oczekiwano 8", len(cut.Days))
	}
	if cut.Smooth != 1 {
		t.Errorf("okno wygladzania = %d, oczekiwano 1", cut.Smooth)
	}
	if cut.ProbeTotal >= full.ProbeTotal {
		t.Errorf("suma sond nie zostala przeliczona: %d vs %d", cut.ProbeTotal, full.ProbeTotal)
	}
	for _, series := range cut.Traffic["fedora"] {
		if len(series) != 8 {
			t.Errorf("seria ruchu ma %d punktow, oczekiwano 8", len(series))
		}
	}
	// The original must stay intact - the remaining views are cut from it.
	if len(full.Days) != 181 {
		t.Error("sliceSnapshot zmodyfikowal zrodlowy snapshot")
	}
}

// The full-window view takes a different path than the cut ones - there is nothing
// to cut. It must still get its own smoothing window and its own copy, because the
// caller writes its own fields into the result while the source is still needed to
// cut the rest.
func TestSliceSnapshotFullWindowIsIndependentCopy(t *testing.T) {
	full := sampleSnapshot(361)
	full.Smooth = 0 // as it is before CollectStats sets it

	view := sliceSnapshot(full, 360)
	if view.Smooth != smoothWindowFor(360) {
		t.Errorf("okno wygladzania = %d, oczekiwano %d", view.Smooth, smoothWindowFor(360))
	}
	if view == full {
		t.Fatal("zwrocono zrodlowy snapshot zamiast kopii")
	}

	view.Views = []int{1, 2, 3}
	if full.Views != nil {
		t.Error("zapis do widoku zmienil snapshot zrodlowy")
	}
}

// Every view must get smoothing chosen for itself, the longest one included.
func TestAllViewsGetOwnSmoothWindow(t *testing.T) {
	full := sampleSnapshot(361)
	sc := NewStatsCollector(nil, 360)
	for _, v := range sc.views {
		if got, want := sliceSnapshot(full, v).Smooth, smoothWindowFor(v); got != want {
			t.Errorf("widok %dd ma okno %d, oczekiwano %d", v, got, want)
		}
	}
}

// The panels over access_log have to count the same endpoints, or the headline
// client number describes a different population than the chart beside it.
//
// This is not hypothetical. The clients panel used to filter with
// endpoint_name != 'releases', from when the release redirect was served here.
// Those rows were moved to releases_access_log and deleted from access_log, so
// the exclusion matched nothing and the panel had silently become "every
// endpoint" while the traffic panel stayed on the named three. A negative
// filter is the wrong shape regardless: a new route would enter the client
// counts without appearing anywhere else on the page.
func TestStatsPanelsAgreeOnEndpoints(t *testing.T) {
	want := "endpoint_name IN (" + endpointInList() + ")"
	for name, query := range map[string]string{
		"traffic": trafficQuery,
		"clients": clientsQuery,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("%s panel does not filter on %s", name, want)
		}
		if strings.Contains(query, "endpoint_name !=") {
			t.Errorf("%s panel excludes an endpoint by name instead of listing the ones it wants", name)
		}
	}
	// The list itself must not name an endpoint this service cannot log.
	for _, ep := range statsEndpoints {
		if ep == "releases" {
			t.Error("statsEndpoints names 'releases', which moved to releases_access_log")
		}
	}
}

// country reaches the page from the CF-IPCountry header. It is believed only
// from a Cloudflare peer, and loopback counts as trusted - which, with the
// container on --network host, means any process on the machine can put an
// arbitrary string in this column. The escaping is the only thing between that
// and stored XSS on a page an operator opens. cmd/releases pins the same
// property for filenames; this is the proxy's equivalent.
func TestCountryPanelEscapesAttackerControlledLabels(t *testing.T) {
	s := sampleSnapshot(30)
	const payload = `<script>alert(1)</script>`
	n := len(s.Days)
	// Large enough to be one of the named bars: a country in the folded tail is
	// rendered as "other (N)" and its label never reaches the page, so a small
	// value here would make the test pass without exercising any escaping.
	req := make([]uint64, n)
	for i := range req {
		req[i] = 100000
	}
	s.Country[payload] = req
	s.CountryClients[payload] = make([]uint64, n)
	s.Countries = append(s.Countries, payload)
	sortCountries(s)

	page := string(renderStats(s))
	if strings.Contains(page, payload) {
		t.Error("the raw payload reached the page unescaped")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("the label is not on the page at all - the escaping test proves nothing")
	}
}

// A flag is derived arithmetically from the letters, so anything that is not
// two ASCII letters would produce a nonsense glyph pair. XX and T1 are real
// values Cloudflare sends and are not countries.
func TestFlagForRejectsNonCountries(t *testing.T) {
	if got := flagFor("PL"); got != "\U0001F1F5\U0001F1F1" {
		t.Errorf("flagFor(PL) = %q, want the Polish flag", got)
	}
	for _, code := range []string{"XX", "T1", "", "P", "POL", "pl", "P1", `<script>`} {
		if got := flagFor(code); got != "" {
			t.Errorf("flagFor(%q) = %q, want no flag", code, got)
		}
	}
}

// "Top 5 share" has to mean five countries. The folded tail row stands for
// dozens, so counting it would silently turn the headline into "top 5 plus
// everything else", which is always close to 100%.
func TestSumTopStopsAtTheFoldedTail(t *testing.T) {
	rows := []countryRow{
		{label: "US", requests: 100, n: 1},
		{label: "PL", requests: 50, n: 1},
		{label: "", requests: 900, n: 40},
	}
	if got := sumTop(rows, 5); got != 150 {
		t.Errorf("sumTop = %d, want 150 (the tail must not count)", got)
	}
}

// The four views come from one collection, so the country panel has to be
// re-derived after slicing rather than carried over: the leader over 360 days
// need not lead over 7, and a total cannot be cut down to a shorter window.
func TestSliceSnapshotRecomputesCountryOrder(t *testing.T) {
	s := sampleSnapshot(60)
	n := len(s.Days)

	// A country that sent nothing for most of the window and then took over.
	// Enough to lead the last week, not enough to lead the whole window - which
	// is the only shape that can tell "re-sorted after slicing" from "carried
	// the full-window order over".
	late := make([]uint64, n)
	for i := n - 5; i < n; i++ {
		late[i] = 400
	}
	s.Country["ZZ"] = late
	s.CountryClients["ZZ"] = make([]uint64, n)
	s.Countries = append(s.Countries, "ZZ")
	sortCountries(s)

	if s.Countries[0] == "ZZ" {
		t.Fatal("ZZ already leads the full window - the test cannot show a difference")
	}
	cut := sliceSnapshot(s, 7)
	if cut.Countries[0] != "ZZ" {
		t.Errorf("over 7 days the leader is %q, want ZZ", cut.Countries[0])
	}
	if got := len(cut.Country["ZZ"]); got != len(cut.Days) {
		t.Errorf("sliced series has %d points, want %d", got, len(cut.Days))
	}
	// The source must not have been mutated - the other views are cut from it.
	if len(s.Country["ZZ"]) != n {
		t.Errorf("slicing mutated the source series: %d points, want %d", len(s.Country["ZZ"]), n)
	}
}

// The folded tail is a line, never a bar.
//
// It is the sum of every country past the twelfth, so on a scale where the
// leading country is full width it is routinely wider than the track. The first
// version drew it at 257%, which .btrack's overflow:hidden clipped to a full
// bar - making the residual look like the single largest origin. Only rendering
// the page showed it.
func TestCountryTailIsNotDrawnAsABar(t *testing.T) {
	s := sampleSnapshot(30)
	n := len(s.Days)
	// Enough countries past countryRows that their sum beats the leader.
	for i := range 30 {
		code := string(rune('A'+i/26)) + string(rune('a'+i%26))
		series := make([]uint64, n)
		for j := range series {
			series[j] = 40
		}
		s.Country[code] = series
		s.CountryClients[code] = make([]uint64, n)
		s.Countries = append(s.Countries, code)
	}
	sortCountries(s)

	page := string(renderStats(s))
	i := strings.Index(page, "Where requests come from")
	if i < 0 {
		t.Fatal("the country section is missing")
	}
	section := page[i:]
	if j := strings.Index(section, `<h2 class="sec"`); j > 0 {
		section = section[:j]
	}

	for _, m := range regexp.MustCompile(`width:([\d.]+)%`).FindAllStringSubmatch(section, -1) {
		w, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatal(err)
		}
		if w > 100 {
			t.Errorf("a bar is %.2f%% wide - it overflows its track", w)
		}
	}
	if !strings.Contains(section, `class="btail"`) {
		t.Error("the tail is missing entirely - it must be reported, just not as a bar")
	}
	if strings.Contains(section, "other (") {
		t.Error("the tail is still rendered as a bar row")
	}
}

// A host that has history but has not been probed for a day is no longer in the
// servers map - elfutils and alpine are commented out there - and the page says
// so beside its name instead of charting it as if it were still live.
func TestProbePanelMarksUnprobedHostsOffline(t *testing.T) {
	s := sampleSnapshot(30)
	page := string(renderStats(s))

	if !s.isOffline("elfutils") || s.isOffline("fedora") {
		t.Fatalf("isOffline: elfutils=%v fedora=%v, want true/false",
			s.isOffline("elfutils"), s.isOffline("fedora"))
	}
	// Four panels name hosts - traffic, bytes, probes and throughput - and the
	// badge has to reach all four. Marking one and not the others makes the same
	// upstream read as live in one section and dead in the next, which is
	// exactly what the first version did.
	sections := map[string]string{
		"traffic":    "Traffic by upstream",
		"bytes":      "Bytes served",
		"probes":     "Resolution probes",
		"throughput": "Upstream throughput",
	}
	for name, heading := range sections {
		i := strings.Index(page, heading)
		if i < 0 {
			t.Errorf("%s section is missing (%q)", name, heading)
			continue
		}
		section := page[i:]
		if j := strings.Index(section[len(heading):], `<h2 class="sec"`); j > 0 {
			section = section[:len(heading)+j]
		}
		e := strings.Index(section, "elfutils")
		if e < 0 {
			t.Errorf("%s section does not name elfutils at all", name)
			continue
		}
		if k := strings.Index(section[e:], `class="off"`); k < 0 || k > 120 {
			t.Errorf("%s section: elfutils carries no offline badge (offset %d)", name, k)
		}
		f := strings.Index(section, "fedora")
		if f >= 0 {
			seg := section[f:min(f+120, len(section))]
			if strings.Contains(seg, `class="off"`) {
				t.Errorf("%s section: fedora is marked offline although it was probed today", name)
			}
		}
	}

	// unresolved is not a host - it stands in for never having reached one - so
	// it is never probed and must never be reported as an outage.
	if s.isOffline(unresolvedLabel) {
		t.Error("unresolved is reported offline; it is the absence of a host, not a dead one")
	}
}

// The guard that keeps this honest. tryAllServers probes every configured
// upstream on every cold build ID, so nobody being probed is the signature of a
// quiet night, not of every backend failing at once. Announcing twelve outages
// because nobody asked for a cold build ID would be worse than saying nothing.
func TestProbePanelSaysNothingWhenNobodyWasProbed(t *testing.T) {
	s := sampleSnapshot(30)
	s.ProbeRecent = map[string]uint64{}
	s.ProbeRecentTotal = 0

	if s.isOffline("elfutils") || s.offlineCount() != 0 {
		t.Error("hosts are reported offline on a day when nothing was probed at all")
	}
	if page := string(renderStats(s)); strings.Contains(page, `class="off"`) {
		t.Error("the page marks hosts offline on a day when nothing was probed")
	}
}
