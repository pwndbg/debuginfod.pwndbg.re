package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	}
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < days; i++ {
		s.Days = append(s.Days, base.AddDate(0, 0, i).Format("2006-01-02"))
	}
	n := len(s.Days)

	for _, host := range []string{unresolvedLabel, "fedora"} {
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
	s.ProbeHosts = []string{"fedora"}
	for _, p := range probes {
		s.ProbeTotal += p.OK + p.Fail
		s.ProbeOK += p.OK
	}
	s.Thru["fedora"] = thru
	s.ThruHosts = []string{"fedora"}
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
