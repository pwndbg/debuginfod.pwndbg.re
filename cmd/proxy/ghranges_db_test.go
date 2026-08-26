package main

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// The CREATE TABLE IF NOT EXISTS in Init is a no-op on a table that already
// exists, so a new column is invisible to every deployed instance unless an
// ALTER accompanies it. That is not hypothetical here - it is exactly how
// apparent_bytes broke cache_stats in production - and access_log is the table
// every request writes to, so getting it wrong takes the service down rather
// than a chart.
func TestDBAccessLogMigrationAddsTagsAndCountry(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	dropAll(t, db)

	// The version from before request classification existed.
	if err := db.conn.Exec(ctx, `
CREATE TABLE access_log (
    timestamp DateTime DEFAULT now(), remote_ip IPv6, method LowCardinality(String),
    endpoint_name LowCardinality(String), request_uri String, status UInt16,
    user_agent String, duration_ms UInt32, duration_100kb_ms UInt32, bytes_sent UInt64,
    buildid String, resolved_host LowCardinality(String), cache_status LowCardinality(String),
    error_msg String,
    response_headers Tuple(size UInt64, file String, archive String, imasignature String)
) ENGINE = MergeTree() PARTITION BY toYYYYMM(timestamp) ORDER BY timestamp`); err != nil {
		t.Fatalf("creating the old table: %v", err)
	}
	cols := columns(t, db, "access_log")
	if _, ok := cols["tags"]; ok {
		t.Fatal("the old table should not have tags")
	}
	if _, ok := cols["country"]; ok {
		t.Fatal("the old table should not have country")
	}

	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cols = columns(t, db, "access_log")
	for _, name := range []string{"tags", "country"} {
		if _, ok := cols[name]; !ok {
			t.Fatalf("Init did not add %s to the existing table", name)
		}
	}

	// Init alone proves nothing - what counts is whether the write goes through.
	err := db.AccessLog(ctx, AccessLogEntry{
		Timestamp: time.Now(), RemoteIP: netip.MustParseAddr("4.148.1.2"),
		Country: "PL", Tags: []string{"github_actions"},
		Method: "GET", EndpointName: "debuginfo", Status: 200,
	})
	if err != nil {
		t.Fatalf("AccessLog after the migration: %v", err)
	}
}

// What /stats counts, now that a row's own tags are the only input. The three
// tag values have to behave differently: CI is dropped, checked-and-not-CI is
// counted, and a row that could not be checked is counted rather than silently
// discarded - it is ordinary traffic until something proves otherwise, and
// scripts/backfill_tags.py can resolve it later.
func TestDBStatsCountsByTagAlone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	dropAll(t, db)
	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	rows := []struct {
		name string
		tags []string
		want bool
	}{
		{"CI", []string{"github_actions"}, false},
		{"checked, not CI", []string{tagDirect}, true},
		{"never checked", []string{tagUnclassified}, true},
	}
	for i, r := range rows {
		err := db.AccessLog(ctx, AccessLogEntry{
			Timestamp: time.Now(), RemoteIP: netip.MustParseAddr("4.148.1.2"),
			Tags: r.tags, Method: "GET", EndpointName: "debuginfo", Status: 200,
			BuildID: string(rune('a' + i)),
		})
		if err != nil {
			t.Fatalf("AccessLog(%s): %v", r.name, err)
		}
	}

	var counted uint64
	q := "SELECT count() FROM access_log WHERE timestamp >= today() - 1" + excludeCI
	if err := db.conn.QueryRow(ctx, q).Scan(&counted); err != nil {
		t.Fatalf("counting: %v", err)
	}
	var want uint64
	for _, r := range rows {
		if r.want {
			want++
		}
	}
	if counted != want {
		t.Errorf("/stats counted %d rows, want %d", counted, want)
	}
}

// The country panel's query, against a real database. It has to agree with the
// panels beside it on both filters: the same three endpoints, and the same CI
// exclusion - a country breakdown that still counted GitHub Actions would be a
// map of Azure regions, not of the people using this service.
func TestDBCollectCountriesFiltersLikeTheOtherPanels(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	dropAll(t, db)
	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	now := time.Now()
	rows := []struct {
		country  string
		endpoint string
		tags     []string
		want     bool
	}{
		{"PL", "debuginfo", []string{tagDirect}, true},
		{"DE", "source", []string{tagUnclassified}, true},
		{"US", "debuginfo", []string{"github_actions"}, false}, // CI
		{"FR", "status", []string{tagDirect}, false},           // not a stats endpoint
		{"", "debuginfo", []string{tagDirect}, false},          // no country: this host talking to itself
	}
	for i, r := range rows {
		err := db.AccessLog(ctx, AccessLogEntry{
			Timestamp: now, RemoteIP: netip.MustParseAddr("93.184.216.34"),
			Country: r.country, Tags: r.tags, Method: "GET",
			EndpointName: r.endpoint, Status: 200, BuildID: string(rune('a' + i)),
		})
		if err != nil {
			t.Fatalf("AccessLog(%s): %v", r.country, err)
		}
	}

	snap := &statsSnapshot{
		Country:        map[string][]uint64{},
		CountryClients: map[string][]uint64{},
	}
	day := now.UTC().Format("2006-01-02")
	snap.Days = []string{day}
	idx := map[string]int{day: 0}
	if err := db.collectCountries(ctx, snap, idx, 1, 1); err != nil {
		t.Fatalf("collectCountries: %v", err)
	}

	got := map[string]bool{}
	for _, c := range snap.Countries {
		got[c] = true
	}
	for _, r := range rows {
		if r.country == "" {
			continue
		}
		if got[r.country] != r.want {
			t.Errorf("country %q present=%v, want %v (endpoint %s, tags %v)",
				r.country, got[r.country], r.want, r.endpoint, r.tags)
		}
	}
	if got[""] {
		t.Error("the empty country reached the panel; it should be dropped, not shown as unknown")
	}
}

// The 24-hour probe count, against a real database. A host whose only rows are
// older than a day must come back at zero, which is what the offline badge
// reads; one probed within the day must not.
func TestDBCollectProbeRecentCountsOnlyTheLastDay(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	dropAll(t, db)
	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	now := time.Now()
	entries := []ResolveLogEntry{
		{Timestamp: now.Add(-1 * time.Hour), BuildID: "a", ResolvedHost: "fedora", Success: true},
		{Timestamp: now.Add(-23 * time.Hour), BuildID: "b", ResolvedHost: "fedora"},
		// Just outside the window, and long outside it: elfutils has history and
		// nothing recent, which is the shape the badge is for.
		{Timestamp: now.Add(-25 * time.Hour), BuildID: "c", ResolvedHost: "elfutils"},
		{Timestamp: now.AddDate(0, 0, -40), BuildID: "d", ResolvedHost: "elfutils"},
	}
	if err := db.ResolveLog(ctx, entries); err != nil {
		t.Fatalf("ResolveLog: %v", err)
	}

	snap := &statsSnapshot{ProbeRecent: map[string]uint64{}}
	if err := db.collectProbeRecent(ctx, snap); err != nil {
		t.Fatalf("collectProbeRecent: %v", err)
	}

	if got := snap.ProbeRecent["fedora"]; got != 2 {
		t.Errorf("fedora probed %d times in the last day, want 2", got)
	}
	if got := snap.ProbeRecent["elfutils"]; got != 0 {
		t.Errorf("elfutils probed %d times in the last day, want 0", got)
	}
	if snap.ProbeRecentTotal != 2 {
		t.Errorf("total %d, want 2", snap.ProbeRecentTotal)
	}

	// And the reading built on top of it.
	snap.ProbeHosts = []string{"fedora", "elfutils"}
	if !snap.isOffline("elfutils") {
		t.Error("elfutils has no probe in the last day but is not reported offline")
	}
	if snap.isOffline("fedora") {
		t.Error("fedora was probed within the day but is reported offline")
	}
	if got := snap.offlineCount(); got != 1 {
		t.Errorf("offlineCount = %d, want 1", got)
	}
}
