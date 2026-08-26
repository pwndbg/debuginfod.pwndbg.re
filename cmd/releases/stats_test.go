package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func makeSnapshot(days int) *releaseStats {
	s := &releaseStats{Window: days, GeneratedAt: time.Unix(1700000000, 0)}
	end := time.Now().UTC().Truncate(24 * time.Hour)
	for i := days; i >= 0; i-- {
		s.Days = append(s.Days, end.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	n := len(s.Days)
	s.Redirects = make([]uint64, n)
	s.IPsPerDay = make([]uint64, n)
	s.GhDaily = make([]uint64, n)
	return s
}

type fakeSource struct {
	mu   sync.Mutex
	err  error
	seen []int
}

func (f *fakeSource) CollectStats(_ context.Context, days int) (*releaseStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, days)
	if f.err != nil {
		return nil, f.err
	}
	return makeSnapshot(days), nil
}

func (f *fakeSource) windows() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.seen...)
}

// Every bar label on this page comes from request_uri, which is whatever the
// client typed - a filename or a version is not validated anywhere. It reaches
// ClickHouse verbatim and comes back out into the HTML, so the escaping is the
// only thing between a crafted request and stored XSS on an admin page.
func TestRenderEscapesAttackerControlledLabels(t *testing.T) {
	s := makeSnapshot(7)
	payload := `<script>alert(1)</script>`
	s.Versions = []labelCount{{payload, 5}}
	s.Assets = []labelCount{{`" onmouseover="alert(2)`, 3}}

	out := string(renderStats(s))
	if strings.Contains(out, payload) {
		t.Error("script tag rendered unescaped")
	}
	if strings.Contains(out, `" onmouseover="`) {
		t.Error("attribute-breaking quote rendered unescaped")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("expected the payload to appear escaped")
	}
}

// renderStats runs on the collector goroutine, where a panic would take the
// process down and only show up as a container restart.
func TestRenderEmptySnapshotDoesNotPanic(t *testing.T) {
	if out := renderStats(&releaseStats{}); len(out) == 0 {
		t.Error("empty snapshot produced no page")
	}
	if out := string(renderStats(makeSnapshot(7))); !strings.Contains(out, "releases.pwndbg.re") {
		t.Error("zero-traffic snapshot did not render")
	}
}

// A window longer than what is collected must not appear in the switcher - it
// would be a tab that can never have data behind it.
func TestViewsAreFilteredToTheCollectedWindow(t *testing.T) {
	sc := NewStatsCollector(&fakeSource{}, 30)
	want := []int{7, 30}
	if len(sc.views) != len(want) {
		t.Fatalf("views = %v, want %v", sc.views, want)
	}
	for i, v := range want {
		if sc.views[i] != v {
			t.Fatalf("views = %v, want %v", sc.views, want)
		}
	}
	if sc.defaultView() != 30 {
		t.Errorf("defaultView = %d, want 30", sc.defaultView())
	}
}

func TestStatsHandlerIsUnavailableBeforeFirstRefresh(t *testing.T) {
	sc := NewStatsCollector(&fakeSource{}, 30)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.Host = serveHost

	sc.Handler(rec, req, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if rec.Header().Get("retry-after") == "" {
		t.Error("no retry-after on the not-ready response")
	}
}

func TestStatsHandlerServesViewsAndRevalidates(t *testing.T) {
	src := &fakeSource{}
	sc := NewStatsCollector(src, 30)
	sc.refresh(context.Background())

	if got := src.windows(); len(got) != 2 {
		t.Fatalf("collected windows %v, want one per view", got)
	}

	get := func(days, ifNone string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/stats?days="+days, nil)
		req.Host = serveHost
		if ifNone != "" {
			req.Header.Set("if-none-match", ifNone)
		}
		rec := httptest.NewRecorder()
		sc.Handler(rec, req, nil)
		return rec
	}

	rec := get("7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "last 7 days") {
		t.Error("?days=7 did not serve the 7-day page")
	}
	etag := rec.Header().Get("etag")
	if etag == "" {
		t.Fatal("no etag")
	}
	if rec := get("7", etag); rec.Code != http.StatusNotModified {
		t.Errorf("conditional request: status %d, want 304", rec.Code)
	}

	// An unknown window must fall back, never trigger a query on demand.
	if rec := get("999", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "last 30 days") {
		t.Errorf("unknown ?days= did not fall back to the default view (status %d)", rec.Code)
	}
	if got := src.windows(); len(got) != 2 {
		t.Errorf("serving pages hit the database again: %v", got)
	}
}

// The page reports who downloads what; it must not answer on a Host that
// cloudflared did not route here, same as the redirect itself.
func TestStatsHandlerRejectsForeignHost(t *testing.T) {
	sc := NewStatsCollector(&fakeSource{}, 30)
	sc.refresh(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	sc.Handler(rec, req, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}

// A failed refresh must leave the previous page in place: stale numbers beat an
// empty page, and beat a 503 the edge might cache.
func TestFailedRefreshKeepsPreviousPage(t *testing.T) {
	src := &fakeSource{}
	sc := NewStatsCollector(src, 30)
	sc.refresh(context.Background())

	src.mu.Lock()
	src.err = errors.New("clickhouse down")
	src.mu.Unlock()
	sc.refresh(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.Host = serveHost
	rec := httptest.NewRecorder()
	sc.Handler(rec, req, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status %d after a failed refresh, want the previous page", rec.Code)
	}
}
