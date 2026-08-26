package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

var ErrDbNoRow = fmt.Errorf("db no row")

func NewDB(ctx context.Context, dsn string) (*dbSrv, error) {
	options, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		return nil, err
	}
	return &dbSrv{
		conn: conn,
	}, nil
}

type dbSrv struct {
	conn driver.Conn
}

func (s *dbSrv) Close() error {
	if conn := s.conn; conn != nil {
		return conn.Close()
	}
	return nil
}

func (s *dbSrv) Init(ctx context.Context) error {
	if err := s.conn.Exec(ctx, `
CREATE TABLE IF NOT EXISTS buildid_state
(
    buildid        String,
    last_host      LowCardinality(String),
    last_error     String,
    counter        UInt32,
    last_success   Bool,
    updated_at     DateTime,
    response_headers Tuple(size UInt64, file String, archive String, imasignature String)  -- x-debuginfod-* headers from the /debuginfo probe against last_host
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY buildid;
`); err != nil {
		return err
	}

	// Migration for instances where buildid_state predates this column.
	if err := s.conn.Exec(ctx, `
ALTER TABLE buildid_state ADD COLUMN IF NOT EXISTS response_headers Tuple(size UInt64, file String, archive String, imasignature String) AFTER updated_at;
`); err != nil {
		return err
	}

	if err := s.conn.Exec(ctx, `
CREATE TABLE IF NOT EXISTS resolve_logs (
    timestamp      DateTime,
    buildid        String,
    resolved_host  LowCardinality(String),
    success        Bool,
    duration_ms    UInt32,
    error_msg      String
) ENGINE = MergeTree()
ORDER BY (timestamp, buildid);
`); err != nil {
		return err
	}

	if err := s.conn.Exec(ctx, `
CREATE TABLE IF NOT EXISTS access_log (
    timestamp     DateTime DEFAULT now(),  -- when the request was handled
    remote_ip     IPv6,                  -- client IP
    method        LowCardinality(String),                  -- GET / POST / etc.
    endpoint_name        LowCardinality(String),                  -- GET / POST / etc.
    request_uri   String,                  -- request path (e.g. /buildid/abc123/executable)
    status        UInt16,                  -- response code (e.g. 200, 404, 500)
    user_agent    String,                  -- User-Agent header
    duration_ms   UInt32,                  -- request handling time in milliseconds
    duration_100kb_ms UInt32,              -- time to deliver the first 100 KiB (0 = response never reached the threshold)
    bytes_sent    UInt64,                  -- bytes written to the client
    buildid       String,                  -- optional: build ID extracted from the URI, if present
    resolved_host LowCardinality(String),                  -- host the bytes actually came from, if proxied
    cache_status  LowCardinality(String),                  -- HIT / MISS / COALESCED / BYPASS / OVERLOADED, empty when the cache was never reached
    error_msg     String,                   -- error description, if any
    response_headers        Tuple(size UInt64, file String, archive String, imasignature String),
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY (timestamp, request_uri);
`); err != nil {
		return err
	}

	// Migration for instances where access_log predates this column - the
	// CREATE TABLE IF NOT EXISTS above is a no-op there, so without this the
	// INSERT would fail.
	if err := s.conn.Exec(ctx, `
ALTER TABLE access_log ADD COLUMN IF NOT EXISTS duration_100kb_ms UInt32 AFTER duration_ms;
`); err != nil {
		return err
	}

	if err := s.conn.Exec(ctx, `
ALTER TABLE access_log ADD COLUMN IF NOT EXISTS cache_status LowCardinality(String) AFTER resolved_host;
`); err != nil {
		return err
	}

	if err := s.conn.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cache_stats (
	timestamp DateTime,
	entries   UInt64,  -- number of blobs in the cache
	bytes     UInt64,  -- blobs + .meta, space ACTUALLY allocated (st_blocks)
	apparent_bytes UInt64,  -- sum of the same files' lengths; differs from bytes on btrfs
	tmp_bytes UInt64,  -- unfinished downloads (.tmp-*); grows when something abandons them
	fs_total  UInt64,  -- capacity of the partition holding CACHE_PATH
	fs_free   UInt64   -- free for non-root (statfs Bavail, excluding the root reserve)
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY timestamp;
`); err != nil {
		return err
	}

	// Migration for instances where cache_stats predates splitting allocated
	// size from apparent size. The CREATE TABLE IF NOT EXISTS above is a no-op
	// there, so without this the INSERT fails with "No such column
	// apparent_bytes".
	if err := s.conn.Exec(ctx, `
ALTER TABLE cache_stats ADD COLUMN IF NOT EXISTS apparent_bytes UInt64 AFTER bytes;
`); err != nil {
		return err
	}

	return nil
}

type BuildIDState struct {
	BuildID     string    `ch:"buildid"`
	LastHost    string    `ch:"last_host"`
	LastError   string    `ch:"last_error"`
	LastSuccess bool      `ch:"last_success"`
	Counter     uint32    `ch:"counter"`
	UpdatedAt   time.Time `ch:"updated_at"`
	// ResponseHeaders comes from the /debuginfo probe against LastHost - it
	// describes the debuginfo artifact, so it says nothing reliable about the
	// executable and source endpoints.
	ResponseHeaders StateHeaders `ch:"response_headers"`
}

func (s *dbSrv) GetState(ctx context.Context, buildID string) (*BuildIDState, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	const query = `
		SELECT buildid, last_host, last_error, counter, last_success, updated_at, response_headers
		FROM buildid_state
		WHERE buildid = ?
		ORDER BY updated_at DESC
		LIMIT 1
	`
	var state BuildIDState
	if err := s.conn.QueryRow(ctx, query, buildID).ScanStruct(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDbNoRow
		}
		return nil, fmt.Errorf("query failed: %w", err)
	}
	return &state, nil
}

func (s *dbSrv) UpdateState(ctx context.Context, state BuildIDState) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	const query = `
		INSERT INTO buildid_state
		(buildid, last_host, last_error, counter, last_success, updated_at, response_headers)
		VALUES (?, ?, ?, ?, ?, ?, tuple(?, ?, ?, ?))
	`
	// NOTE: AsyncInsert builds a textual VALUES clause, so it cannot serialise a Go
	// struct into a Tuple - StateHeaders would render as "{42 f a s}" and ClickHouse
	// would reject the row. Hence tuple(?, ?, ?, ?) and four separate scalars.
	err := s.conn.AsyncInsert(ctx, query, false,
		state.BuildID,
		state.LastHost,
		state.LastError,
		state.Counter,
		state.LastSuccess,
		state.UpdatedAt,
		state.ResponseHeaders.Size,
		state.ResponseHeaders.File,
		state.ResponseHeaders.Archive,
		state.ResponseHeaders.ImaSignature,
	)
	if err != nil {
		return fmt.Errorf("insert failed: %w", err)
	}
	return nil
}

type ResolveLogEntry struct {
	Timestamp    time.Time
	BuildID      string
	ResolvedHost string
	Success      bool
	Duration     time.Duration
	ErrorMsg     string
}

func (s *dbSrv) ResolveLog(ctx context.Context, entries []ResolveLogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	batch, err := s.conn.PrepareBatch(ctx, `
		INSERT INTO resolve_logs (
			timestamp,
			buildid,
			resolved_host,
			success,
			duration_ms,
			error_msg
		)
	`)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if err := batch.Append(
			e.Timestamp,
			e.BuildID,
			e.ResolvedHost,
			e.Success,
			uint32(e.Duration.Milliseconds()),
			e.ErrorMsg,
		); err != nil {
			return err
		}
	}
	if err := batch.Send(); err != nil {
		return err
	}

	return nil
}

type StateHeaders struct {
	Size         uint64 `ch:"size"`
	File         string `ch:"file"`
	Archive      string `ch:"archive"`
	ImaSignature string `ch:"imasignature"`
}

type AccessLogEntry struct {
	Timestamp       time.Time
	RemoteIP        netip.Addr
	Method          string
	EndpointName    string
	RequestURI      string
	Status          uint16
	UserAgent       string
	Duration        time.Duration
	Duration100kb   time.Duration
	BytesSent       uint64
	BuildID         string
	ResolvedHost    string
	CacheStatus     string
	ErrorMsg        string
	ResponseHeaders StateHeaders
}

func (s *dbSrv) AccessLog(ctx context.Context, entry AccessLogEntry) error {
	ctx = clickhouse.Context(ctx, clickhouse.WithStdAsync(false))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	const query = `
		INSERT INTO access_log 
		(timestamp, remote_ip, method, endpoint_name, request_uri, status, user_agent, duration_ms, duration_100kb_ms, bytes_sent, buildid, resolved_host, cache_status, error_msg, response_headers)
	`
	batch, err := s.conn.PrepareBatch(ctx, query)
	if err != nil {
		return err
	}

	if err := batch.Append(
		entry.Timestamp,
		entry.RemoteIP,
		entry.Method,
		entry.EndpointName,
		entry.RequestURI,
		entry.Status,
		entry.UserAgent,
		uint32(entry.Duration.Milliseconds()),
		uint32(entry.Duration100kb.Milliseconds()),
		entry.BytesSent,
		entry.BuildID,
		entry.ResolvedHost,
		entry.CacheStatus,
		entry.ErrorMsg,
		entry.ResponseHeaders,
	); err != nil {
		return err
	}

	if err = batch.Send(); err != nil {
		return err
	}
	return nil
}

// InsertCacheUsage writes one measurement. One row every few minutes, so
// PrepareBatch rather than AsyncInsert: the latter with wait=false swallows
// server errors, and at this write rate we want to know when it stops working.
func (s *dbSrv) InsertCacheUsage(ctx context.Context, u cacheUsage) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	batch, err := s.conn.PrepareBatch(ctx, `
		INSERT INTO cache_stats (timestamp, entries, bytes, apparent_bytes, tmp_bytes, fs_total, fs_free)
	`)
	if err != nil {
		return err
	}
	if err := batch.Append(u.Timestamp, u.Entries, u.Bytes, u.ApparentBytes,
		u.TmpBytes, u.FsTotal, u.FsFree); err != nil {
		return err
	}
	return batch.Send()
}

func errToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
