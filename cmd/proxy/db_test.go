package main

import (
	"context"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// ClickHouse integration tests. Skipped by default so `go test ./...` works
// without docker:
//
//	docker run -d --name ch -p 19000:9000 \
//	  -e CLICKHOUSE_USER=cypis -e CLICKHOUSE_PASSWORD=cypis \
//	  clickhouse/clickhouse-server:25.3-alpine
//	TEST_CLICKHOUSE_DSN='clickhouse://127.0.0.1:19000?username=cypis&password=cypis' \
//	  go test ./cmd/proxy -run TestDB -v
func testDB(t *testing.T) *dbSrv {
	t.Helper()
	dsn := os.Getenv("TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("set TEST_CLICKHOUSE_DSN to run the ClickHouse integration tests")
	}
	// dropAll below destroys buildid_state, access_log, resolve_logs and
	// cache_stats - the entire production dataset, under exactly those names. Port
	// 9000 is production ClickHouse and is reachable from a dev machine, so a DSN
	// pointing there is refused rather than trusted.
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

func dropAll(t *testing.T, db *dbSrv) {
	t.Helper()
	for _, table := range []string{"buildid_state", "access_log", "resolve_logs", "cache_stats"} {
		if err := db.conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
}

func columns(t *testing.T, db *dbSrv, table string) map[string]string {
	t.Helper()
	rows, err := db.conn.Query(context.Background(),
		"SELECT name, type FROM system.columns WHERE database = currentDatabase() AND table = ?", table)
	if err != nil {
		t.Fatalf("odczyt kolumn %s: %v", table, err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		out[name] = typ
	}
	return out
}

// AsyncInsert with wait=false reports no errors, so a flush has to be forced
// before reading - otherwise the test would see a missing row instead of the
// real cause.
func flushAsync(t *testing.T, db *dbSrv) {
	t.Helper()
	if err := db.conn.Exec(context.Background(), "SYSTEM FLUSH ASYNC INSERT QUEUE"); err != nil {
		time.Sleep(2 * time.Second)
	}
}

func TestDBInitCreatesSchemaAndIsIdempotent(t *testing.T) {
	db := testDB(t)
	dropAll(t, db)
	ctx := context.Background()

	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init na pustej bazie: %v", err)
	}
	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init drugi raz (musi byc idempotentny): %v", err)
	}

	state := columns(t, db, "buildid_state")
	if _, ok := state["response_headers"]; !ok {
		t.Errorf("buildid_state bez kolumny response_headers: %v", state)
	}
	access := columns(t, db, "access_log")
	if _, ok := access["duration_100kb_ms"]; !ok {
		t.Errorf("access_log bez kolumny duration_100kb_ms: %v", access)
	}
}

// CREATE TABLE IF NOT EXISTS is a no-op on an existing table, so without the
// ALTERs an instance predating those columns would start failing on INSERT.
func TestDBMigratesLegacyTables(t *testing.T) {
	db := testDB(t)
	dropAll(t, db)
	ctx := context.Background()

	// the schema from before response_headers / duration_100kb_ms were added
	if err := db.conn.Exec(ctx, `
CREATE TABLE buildid_state (
    buildid String, last_host LowCardinality(String), last_error String,
    counter UInt32, last_success Bool, updated_at DateTime
) ENGINE = ReplacingMergeTree(updated_at) ORDER BY buildid`); err != nil {
		t.Fatal(err)
	}
	if err := db.conn.Exec(ctx, `
CREATE TABLE access_log (
    timestamp DateTime DEFAULT now(), remote_ip IPv6, method LowCardinality(String),
    endpoint_name LowCardinality(String), request_uri String, status UInt16,
    user_agent String, duration_ms UInt32, bytes_sent UInt64, buildid String,
    resolved_host LowCardinality(String), error_msg String,
    response_headers Tuple(size UInt64, file String, archive String, imasignature String)
) ENGINE = MergeTree() PARTITION BY toYYYYMM(timestamp) ORDER BY (timestamp, request_uri)`); err != nil {
		t.Fatal(err)
	}
	if err := db.conn.Exec(ctx,
		`INSERT INTO buildid_state VALUES ('stary', 'debian', '', 3, 1, '2026-04-02 17:43:13')`); err != nil {
		t.Fatal(err)
	}

	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init na starych tabelach: %v", err)
	}

	if _, ok := columns(t, db, "buildid_state")["response_headers"]; !ok {
		t.Error("ALTER nie dodal response_headers do buildid_state")
	}
	if _, ok := columns(t, db, "access_log")["duration_100kb_ms"]; !ok {
		t.Error("ALTER nie dodal duration_100kb_ms do access_log")
	}

	// A historical row must survive the migration untouched.
	old, err := db.GetState(ctx, "stary")
	if err != nil {
		t.Fatalf("odczyt historycznego wiersza: %v", err)
	}
	if old.LastHost != "debian" || old.Counter != 3 || !old.LastSuccess {
		t.Errorf("ALTER uszkodzil historyczny wiersz: %+v", old)
	}
	if (old.ResponseHeaders != StateHeaders{}) {
		t.Errorf("historyczny wiersz powinien miec puste naglowki: %+v", old.ResponseHeaders)
	}
}

// AsyncInsert builds a textual VALUES clause and can NOT serialise a Go struct
// into a Tuple - which is why UpdateState uses tuple(?, ?, ?, ?). With wait=false
// the error never reaches the client, so a regression would be silent: the column
// would simply be empty.
func TestDBStateHeadersRoundTrip(t *testing.T) {
	db := testDB(t)
	dropAll(t, db)
	ctx := context.Background()
	if err := db.Init(ctx); err != nil {
		t.Fatal(err)
	}

	want := BuildIDState{
		BuildID:     "aabbccddeeff00112233445566778899",
		LastHost:    "fedora",
		LastSuccess: true,
		UpdatedAt:   time.Now().Truncate(time.Second),
		ResponseHeaders: StateHeaders{
			Size:         4538432,
			File:         "/usr/lib/debug/usr/bin/bash.debug",
			Archive:      "/mnt/koji/bash-debuginfo-5.3.0-2.fc43.aarch64.rpm",
			ImaSignature: "030204d0431c32",
		},
	}
	if err := db.UpdateState(ctx, want); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	flushAsync(t, db)

	got, err := db.GetState(ctx, want.BuildID)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if got.ResponseHeaders != want.ResponseHeaders {
		t.Errorf("naglowki nie przetrwaly round-tripu:\n chciano %+v\n dostano %+v",
			want.ResponseHeaders, got.ResponseHeaders)
	}
	if got.LastHost != want.LastHost || !got.LastSuccess {
		t.Errorf("stan uszkodzony: %+v", got)
	}
}

func TestDBGetStateMissingRow(t *testing.T) {
	db := testDB(t)
	dropAll(t, db)
	ctx := context.Background()
	if err := db.Init(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := db.GetState(ctx, "nie-ma-takiego-buildid"); err != ErrDbNoRow {
		t.Errorf("oczekiwano ErrDbNoRow, dostano %v", err)
	}
}

func TestDBAccessLogRoundTrip(t *testing.T) {
	db := testDB(t)
	dropAll(t, db)
	ctx := context.Background()
	if err := db.Init(ctx); err != nil {
		t.Fatal(err)
	}

	entry := AccessLogEntry{
		Timestamp:     time.Now().Truncate(time.Second),
		RemoteIP:      netip.MustParseAddr("2001:db8::1"),
		Method:        "GET",
		EndpointName:  "debuginfo",
		RequestURI:    "/buildid/aabb/debuginfo",
		Status:        200,
		UserAgent:     "elfutils/0.192",
		Duration:      1500 * time.Millisecond,
		Duration100kb: 120 * time.Millisecond,
		BytesSent:     4538432,
		BuildID:       "aabb",
		ResolvedHost:  "fedora",
	}
	if err := db.AccessLog(ctx, entry); err != nil {
		t.Fatalf("AccessLog: %v", err)
	}

	var durMs, dur100kbMs uint32
	var bytesSent uint64
	var status uint16
	if err := db.conn.QueryRow(ctx,
		`SELECT duration_ms, duration_100kb_ms, bytes_sent, status FROM access_log LIMIT 1`).
		Scan(&durMs, &dur100kbMs, &bytesSent, &status); err != nil {
		t.Fatalf("odczyt: %v", err)
	}

	if durMs != 1500 {
		t.Errorf("duration_ms=%d, oczekiwano 1500", durMs)
	}
	if dur100kbMs != 120 {
		t.Errorf("duration_100kb_ms=%d, oczekiwano 120", dur100kbMs)
	}
	if bytesSent != 4538432 || status != 200 {
		t.Errorf("bytes_sent=%d status=%d", bytesSent, status)
	}
}

func TestDBResolveLogBatch(t *testing.T) {
	db := testDB(t)
	dropAll(t, db)
	ctx := context.Background()
	if err := db.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// An empty list must not go to the database as an empty batch.
	if err := db.ResolveLog(ctx, nil); err != nil {
		t.Errorf("pusta lista: %v", err)
	}

	entries := []ResolveLogEntry{
		{Timestamp: time.Now(), BuildID: "aa", ResolvedHost: "fedora", Success: true, Duration: 50 * time.Millisecond},
		{Timestamp: time.Now(), BuildID: "aa", ResolvedHost: "debian", Success: false, Duration: 80 * time.Millisecond, ErrorMsg: "not found"},
	}
	if err := db.ResolveLog(ctx, entries); err != nil {
		t.Fatalf("ResolveLog: %v", err)
	}

	var count uint64
	if err := db.conn.QueryRow(ctx, `SELECT count() FROM resolve_logs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("zapisano %d wierszy, oczekiwano 2", count)
	}
}

// The cache_status column must be created both on a fresh database and via ALTER
// on a table predating it - and survive a round trip.
func TestDBCacheStatusColumn(t *testing.T) {
	db := testDB(t)
	dropAll(t, db)
	ctx := context.Background()

	// the table shaped as it was BEFORE the column was added
	if err := db.conn.Exec(ctx, `
CREATE TABLE access_log (
    timestamp DateTime DEFAULT now(), remote_ip IPv6, method LowCardinality(String),
    endpoint_name LowCardinality(String), request_uri String, status UInt16,
    user_agent String, duration_ms UInt32, duration_100kb_ms UInt32, bytes_sent UInt64,
    buildid String, resolved_host LowCardinality(String), error_msg String,
    response_headers Tuple(size UInt64, file String, archive String, imasignature String)
) ENGINE = MergeTree() PARTITION BY toYYYYMM(timestamp) ORDER BY (timestamp, request_uri)`); err != nil {
		t.Fatal(err)
	}

	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init na tabeli bez cache_status: %v", err)
	}
	if _, ok := columns(t, db, "access_log")["cache_status"]; !ok {
		t.Fatal("ALTER nie dodal kolumny cache_status")
	}

	for _, status := range []string{"HIT", "MISS", "COALESCED", "BYPASS", "OVERLOADED", ""} {
		if err := db.AccessLog(ctx, AccessLogEntry{
			Timestamp: time.Now().Truncate(time.Second), RemoteIP: netip.MustParseAddr("::1"),
			Method: "GET", EndpointName: "debuginfo", RequestURI: "/x", Status: 200,
			CacheStatus: status, ResolvedHost: "fedora",
		}); err != nil {
			t.Fatalf("AccessLog(%q): %v", status, err)
		}
	}

	var hits, total uint64
	if err := db.conn.QueryRow(ctx,
		`SELECT countIf(cache_status = 'HIT'), count() FROM access_log`).Scan(&hits, &total); err != nil {
		t.Fatal(err)
	}
	if total != 6 || hits != 1 {
		t.Errorf("odczytano hits=%d total=%d, oczekiwano 1/6", hits, total)
	}
	t.Logf("hit rate policzalny z ClickHouse: %d/%d", hits, total)
}

// cache_stats was deployed without apparent_bytes, and CREATE TABLE IF NOT EXISTS
// does nothing on an existing table - every INSERT then failed with
// "No such column apparent_bytes". This test reproduces exactly that state.
func TestDBCacheStatsMigrationAddsApparentBytes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	dropAll(t, db)

	// The version from before allocated size was split from apparent size.
	if err := db.conn.Exec(ctx, `
CREATE TABLE cache_stats (
	timestamp DateTime, entries UInt64, bytes UInt64,
	tmp_bytes UInt64, fs_total UInt64, fs_free UInt64
) ENGINE = MergeTree() PARTITION BY toYYYYMM(timestamp) ORDER BY timestamp`); err != nil {
		t.Fatalf("tworzenie starej tabeli: %v", err)
	}
	if _, ok := columns(t, db, "cache_stats")["apparent_bytes"]; ok {
		t.Fatal("stara tabela nie powinna miec apparent_bytes")
	}

	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, ok := columns(t, db, "cache_stats")["apparent_bytes"]; !ok {
		t.Fatal("Init nie dodal apparent_bytes do istniejacej tabeli")
	}

	// Init alone proves nothing - what counts is whether the write goes through.
	u := cacheUsage{
		Timestamp: time.Now(), Entries: 7, Bytes: 4096,
		ApparentBytes: 3000, TmpBytes: 512, FsTotal: 1 << 40, FsFree: 1 << 39,
	}
	if err := db.InsertCacheUsage(ctx, u); err != nil {
		t.Fatalf("InsertCacheUsage po migracji: %v", err)
	}

	var gotBytes, gotApparent uint64
	row := db.conn.QueryRow(ctx, "SELECT bytes, apparent_bytes FROM cache_stats LIMIT 1")
	if err := row.Scan(&gotBytes, &gotApparent); err != nil {
		t.Fatalf("odczyt: %v", err)
	}
	if gotBytes != 4096 || gotApparent != 3000 {
		t.Errorf("odczytano bytes=%d apparent=%d, oczekiwano 4096/3000", gotBytes, gotApparent)
	}
}

// On a clean instance the CREATE must already contain the column, so the ALTER is
// purely a migration path and not a hidden requirement for working at all.
func TestDBCacheStatsFreshCreateHasAllColumns(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	dropAll(t, db)

	if err := db.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cols := columns(t, db, "cache_stats")
	for _, want := range []string{"timestamp", "entries", "bytes", "apparent_bytes",
		"tmp_bytes", "fs_total", "fs_free"} {
		if _, ok := cols[want]; !ok {
			t.Errorf("brak kolumny %q w swiezo utworzonej cache_stats", want)
		}
	}
}
