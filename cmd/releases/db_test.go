package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// These run only with a throwaway ClickHouse:
//
//	docker run -d --name ch-test -p 19000:9000 \
//	  -e CLICKHOUSE_USER=cypis -e CLICKHOUSE_PASSWORD=cypis \
//	  clickhouse/clickhouse-server:25.3-alpine
//	TEST_CLICKHOUSE_DSN='clickhouse://127.0.0.1:19000?username=cypis&password=cypis' \
//	  go test ./cmd/releases -run TestDB -v
//
// Never point this at 127.0.0.1:9000 - that is production.
func testDB(t *testing.T) *dbSrv {
	t.Helper()
	dsn := os.Getenv("TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("set TEST_CLICKHOUSE_DSN to run ClickHouse-backed tests")
	}
	// These tests DROP tables whose names are the production ones. Port 9000 is
	// production ClickHouse and is reachable from a dev machine, so a DSN pointing
	// there would destroy real data - refuse it rather than trust whoever exported
	// the variable.
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse TEST_CLICKHOUSE_DSN: %v", err)
	}
	for _, addr := range opts.Addr {
		if _, port, err := net.SplitHostPort(addr); err == nil && port == "9000" {
			t.Fatalf("refusing to run destructive tests against %s - port 9000 is production; "+
				"use a throwaway instance on another port", addr)
		}
	}

	db, err := NewDB(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func dropTables(t *testing.T, db *dbSrv) {
	t.Helper()
	for _, tbl := range []string{"releases_access_log", "github_download_stats"} {
		if err := db.conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+tbl); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
}

func TestDBInitIsIdempotent(t *testing.T) {
	db := testDB(t)
	dropTables(t, db)
	for i := range 2 {
		if err := db.Init(context.Background()); err != nil {
			t.Fatalf("Init #%d: %v", i+1, err)
		}
	}
}

// Exercises every query CollectStats runs. Without this the SQL is only checked
// in production, where a typo means /stats stays blank and the reason is a log
// line nobody reads.
func TestDBCollectStats(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	dropTables(t, db)
	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	now := time.Now()
	rows := []struct {
		version, file, ua, country string
		status                     uint16
	}{
		{"2026.02.18", "pwndbg_2026.02.18_amd64.deb", "pwndbg-installer/1.0", "PL", 302},
		{"2026.02.18", "pwndbg_2026.02.18_amd64.deb", "curl/8.5.0", "DE", 302},
		{"2026.02.18", "pwndbg_2026.02.18_macos_arm64-portable.tar.xz", "Homebrew/4.2 (arm64)", "US", 302},
		{"2026.02.18", "pwndbg-lldb_2026.02.18_x86_64-portable.tar.xz", "Homebrew/4.2 (Intel)", "US", 302},
		// Not version-shaped: must be bucketed, never shown as its own bar.
		{"../../etc/passwd", "pwndbg_2026.02.18_amd64.deb", "curl/8.5.0", "FR", 302},
		// A foreign Host: logged, but must not reach any bucket.
		{"2026.02.18", "pwndbg_2026.02.18_amd64.deb", "curl/8.5.0", "", 404},
	}
	for i, r := range rows {
		if err := db.AccessLog(ctx, AccessLogEntry{
			Timestamp:  now,
			RemoteIP:   netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", i+1)),
			Country:    r.country,
			Version:    r.version,
			File:       r.file,
			RequestURI: "/releases/" + r.version + "/" + r.file,
			Status:     r.status,
			UserAgent:  r.ua,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// The artifact breakdown comes from here, not from the access log above: our
	// log only sees clients that came through this host. The two are seeded so
	// they disagree - the access log is heavy on .deb, GitHub is not - so an
	// assertion below can only pass if the right source was used.
	if err := db.GhDownloadStats(ctx, []DownloadStats{
		{now.Add(-2 * time.Hour), "2026.02.18", "pwndbg_2026.02.18_amd64.deb", 100},
		{now.Add(-1 * time.Hour), "2026.02.18", "pwndbg_2026.02.18_amd64.deb", 140},
		{now.Add(-2 * time.Hour), "2026.02.18", "pwndbg_2026.02.18_macos_arm64-portable.tar.xz", 50},
		{now.Add(-1 * time.Hour), "2026.02.18", "pwndbg_2026.02.18_macos_arm64-portable.tar.xz", 60},
		{now.Add(-2 * time.Hour), "2026.02.18", "pwndbg-lldb_2026.02.18_x86_64-portable.tar.xz", 20},
		{now.Add(-1 * time.Hour), "2026.02.18", "pwndbg-lldb_2026.02.18_x86_64-portable.tar.xz", 25},
		// A superseded release. Its artifacts must not be mixed in: it stopped
		// being polled when 2026.02.18 shipped, so it was counted over a
		// different span and is not comparable.
		{now.Add(-90 * 24 * time.Hour), "2025.01.01", "pwndbg_2025.01.01_riscv64.deb", 900},
	}); err != nil {
		t.Fatalf("gh stats: %v", err)
	}

	snap, err := db.CollectStats(ctx, 7)
	if err != nil {
		t.Fatalf("CollectStats: %v", err)
	}

	if snap.Total != 5 {
		t.Errorf("Total = %d, want 5 (the 404 must be excluded)", snap.Total)
	}
	if snap.UniqIPs != 5 {
		t.Errorf("UniqIPs = %d, want 5", snap.UniqIPs)
	}
	if snap.UniqCountries != 4 {
		t.Errorf("UniqCountries = %d, want 4", snap.UniqCountries)
	}
	// Artifact buckets: GitHub's counters for the latest release only.
	// linux/x86_64 = 140 (.deb) + 25 (lldb portable); macos/arm64 = 60.
	if got := labelValue(snap.Platforms, "linux/x86_64"); got != 165 {
		t.Errorf("linux/x86_64 = %d, want 165 from github_download_stats", got)
	}
	if got := labelValue(snap.Platforms, "macos/arm64"); got != 60 {
		t.Errorf("macos/arm64 = %d, want 60", got)
	}
	if got := labelValue(snap.Variants, "pwndbg-lldb"); got != 25 {
		t.Errorf("pwndbg-lldb = %d, want 25", got)
	}
	if got := labelValue(snap.Variants, "pwndbg-gdb"); got != 200 {
		t.Errorf("pwndbg-gdb = %d, want 200", got)
	}
	if got := labelValue(snap.Formats, ".deb"); got != 140 {
		t.Errorf(".deb = %d, want 140", got)
	}
	if got := labelValue(snap.Assets, "pwndbg_2026.02.18_amd64.deb"); got != 140 {
		t.Errorf("top asset = %d, want 140", got)
	}
	// The superseded release must contribute nothing.
	if got := labelValue(snap.Assets, "pwndbg_2025.01.01_riscv64.deb"); got != 0 {
		t.Errorf("an artifact from a superseded release leaked in (%d)", got)
	}
	if got := labelValue(snap.Platforms, unknownBucket); got != 0 {
		t.Errorf("unexpected 'other' platform (%d) - the riscv asset from the old tag leaked in", got)
	}
	if got := labelValue(snap.Versions, "2026.02.18"); got != 4 {
		t.Errorf("version bucket = %d, want 4", got)
	}
	// The path-traversal-looking version is not version-shaped, so it must be
	// folded away rather than rendered as a bar labelled with what a client sent.
	if got := labelValue(snap.Versions, "../../etc/passwd"); got != 0 {
		t.Errorf("a non-version string got its own bar (%d)", got)
	}
	if got := labelValue(snap.Versions, unknownBucket); got != 1 {
		t.Errorf("other = %d, want 1", got)
	}
	if got := labelValue(snap.Clients, "Homebrew arm64"); got != 1 {
		t.Errorf("Homebrew arm64 = %d, want 1", got)
	}
	if got := labelValue(snap.Clients, "Homebrew intel"); got != 1 {
		t.Errorf("Homebrew intel = %d, want 1", got)
	}
	if got := labelValue(snap.Clients, "pwndbg-installer"); got != 1 {
		t.Errorf("pwndbg-installer = %d, want 1", got)
	}
	// curl is no longer its own bucket.
	if got := labelValue(snap.Clients, "curl"); got != 0 {
		t.Errorf("curl got its own bucket (%d); the vocabulary is meant to be closed", got)
	}
	if got := labelValue(snap.Clients, unknownBucket); got != 2 {
		t.Errorf("other = %d, want 2 (the two curl clients)", got)
	}

	// The first sample of a (tag, asset) is dropped, so only the +40 delta counts.
	var ghSum uint64
	for _, v := range snap.GhDaily {
		ghSum += v
	}
	if ghSum != 55 {
		t.Errorf("GhDaily sum = %d, want 55 (40+10+5; first samples are not a day)", ghSum)
	}
	if snap.GhTotal != 1125 {
		t.Errorf("GhTotal = %d, want 1125 (all releases, including the superseded one)", snap.GhTotal)
	}
	if snap.GhLatestTag != "2026.02.18" {
		t.Errorf("latest tag = %q, want 2026.02.18", snap.GhLatestTag)
	}

	// Rendering the real snapshot catches a length mismatch between a column and
	// the day axis, which would panic on the collector goroutine.
	if len(renderStats(snap)) == 0 {
		t.Error("rendering the collected snapshot produced nothing")
	}
}

func labelValue(items []labelCount, label string) uint64 {
	for _, it := range items {
		if it.Label == label {
			return it.N
		}
	}
	return 0
}

// Release curves are the only way to compare two releases fairly: the counter runs
// only while a tag is `latest`, so a total mostly reports how long the tag stayed
// current. This pins the three things that make the curve trustworthy - a gap in
// polling carries forward instead of collapsing, a counter that goes backwards is
// ignored, and a tag seen once produces no curve at all.
func TestDBReleaseCurves(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	dropTables(t, db)
	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	day := func(ago int) time.Time { return time.Now().AddDate(0, 0, -ago) }
	var rows []DownloadStats
	add := func(tag string, ago int, n int) {
		rows = append(rows, DownloadStats{day(ago), tag, tag + "_asset.deb", n})
	}

	// Old release: 41 days of sampling, starting 60 days ago.
	for i := 0; i <= 40; i++ {
		switch i {
		case 5: // a missed poll - no row at all
			continue
		case 7: // GitHub revised the counter downwards
			add("2026.01.01", 60-i, 130)
		default:
			add("2026.01.01", 60-i, 100+10*i)
		}
	}
	// Current release: only 10 days old, so it has not reached the horizon.
	for i := 0; i <= 10; i++ {
		add("2026.06.01", 10-i, 500+20*i)
	}
	// Seen once, long after it stopped being latest.
	add("2025.01.01", 300, 9999)

	if err := db.GhDownloadStats(ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	snap, err := db.CollectStats(ctx, 7)
	if err != nil {
		t.Fatalf("CollectStats: %v", err)
	}

	if len(snap.Curves) != 2 {
		t.Fatalf("%d curves, want 2 (the single-sample tag has none)", len(snap.Curves))
	}
	old, cur := snap.Curves[0], snap.Curves[1]
	if old.Tag != "2026.01.01" || cur.Tag != "2026.06.01" {
		t.Fatalf("curves = %q, %q; want oldest first", old.Tag, cur.Tag)
	}

	if old.Series[0] != 0 {
		t.Errorf("day 0 = %v, want 0 - the baseline must be removed", old.Series[0])
	}
	for i := 1; i < len(old.Series); i++ {
		if old.Series[i] < old.Series[i-1] {
			t.Fatalf("curve decreases at day %d (%v -> %v); a cumulative counter cannot",
				i, old.Series[i-1], old.Series[i])
		}
	}
	// day 4 = 140, day 5 missing -> carried forward, day 6 = 160.
	if old.Series[5] != 40 {
		t.Errorf("day 5 (missed poll) = %v, want 40 carried over from day 4", old.Series[5])
	}
	// day 6 = 160, day 7 revised down to 130 -> ignored.
	if old.Series[7] != 60 {
		t.Errorf("day 7 (counter went backwards) = %v, want 60 held from day 6", old.Series[7])
	}
	if !old.Reached || old.AtHorizon != 300 {
		t.Errorf("horizon: reached=%v value=%d, want true/300", old.Reached, old.AtHorizon)
	}
	if cur.Reached {
		t.Error("a 10-day-old release must not claim a 30-day figure")
	}
	if cur.AtHorizon != cur.Total {
		t.Errorf("young release: AtHorizon=%d Total=%d, want them equal", cur.AtHorizon, cur.Total)
	}
	// Clipped for drawing, but Total still reports the last value ever seen -
	// the two must not be confused.
	if len(old.Series) != curveHorizon+1 {
		t.Errorf("series length = %d, want %d (clipped to the horizon)", len(old.Series), curveHorizon+1)
	}
	if old.Total != 400 {
		t.Errorf("Total = %d, want 400 (day 40, past the horizon)", old.Total)
	}
	if snap.CurveDays != curveHorizon+1 {
		t.Errorf("CurveDays = %d, want %d", snap.CurveDays, curveHorizon+1)
	}
}
