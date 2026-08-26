package main

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

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
	return &dbSrv{conn: conn}, nil
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

// releases_access_log holds one row per release redirect.
//
// It is NOT a copy of cmd/proxy's access_log. Most of that table describes
// proxying debuginfo - the upstream that answered, the cache decision, the
// debuginfod response headers, how long the first 100 KiB took - and none of it
// exists for a 302 that carries no payload. Those columns are dropped rather than
// carried along empty; MIGRATION.sql maps the ones that do transfer.
//
// version and file come from the route parameters, not from parsing request_uri:
// httprouter has already split the path, so storing its own match is exact and
// costs nothing. request_uri is kept anyway, verbatim, because it is the only
// record of anything the two columns discard - a query string, odd escaping.
const createAccessLog = `
CREATE TABLE IF NOT EXISTS releases_access_log (
    timestamp   DateTime DEFAULT now(),  -- when the request was handled
    remote_ip   IPv6,                    -- client IP, from CF-Connecting-IP
    country     LowCardinality(String),  -- CF-IPCountry, '' when Cloudflare does not send it
    version     String,                  -- release tag asked for
    file        String,                  -- asset name asked for
    request_uri String,                  -- verbatim, incl. any query string
    status      UInt16,                  -- 302 on a redirect, 404 on a foreign Host
    user_agent  String                   -- how Homebrew's architecture is identified
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY (timestamp, version, file);
`

// Init creates the tables this service owns. cmd/proxy no longer does - the
// service that writes a table is the one that should create it.
//
// Any column added here later needs its own ALTER TABLE ... ADD COLUMN IF NOT
// EXISTS beside the CREATE: on a table that already exists, CREATE TABLE IF NOT
// EXISTS is a no-op and silently does not add it, which is how apparent_bytes
// broke cache_stats in production.
func (s *dbSrv) Init(ctx context.Context) error {
	stmts := []string{
		`
CREATE TABLE IF NOT EXISTS github_download_stats (
	timestamp DateTime,
	release_tag String,
	asset_name String,
	download_count UInt32
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY (timestamp, release_tag, asset_name);
`,
		createAccessLog,
	}
	for _, stmt := range stmts {
		if err := s.conn.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

type AccessLogEntry struct {
	Timestamp  time.Time
	RemoteIP   netip.Addr
	Country    string
	Version    string
	File       string
	RequestURI string
	Status     uint16
	UserAgent  string
}

func (s *dbSrv) AccessLog(ctx context.Context, entry AccessLogEntry) error {
	ctx = clickhouse.Context(ctx, clickhouse.WithStdAsync(false))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	batch, err := s.conn.PrepareBatch(ctx, `
		INSERT INTO releases_access_log
		(timestamp, remote_ip, country, version, file, request_uri, status, user_agent)
	`)
	if err != nil {
		return err
	}
	if err := batch.Append(
		entry.Timestamp,
		entry.RemoteIP,
		entry.Country,
		entry.Version,
		entry.File,
		entry.RequestURI,
		entry.Status,
		entry.UserAgent,
	); err != nil {
		return err
	}
	return batch.Send()
}

func (s *dbSrv) GhDownloadStats(ctx context.Context, entries []DownloadStats) error {
	if len(entries) == 0 {
		return nil
	}

	ctx = clickhouse.Context(ctx, clickhouse.WithStdAsync(false))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	batch, err := s.conn.PrepareBatch(ctx, `
		INSERT INTO github_download_stats (timestamp, release_tag, asset_name, download_count)
	`)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, entry := range entries {
		if err := batch.Append(entry.Timestamp, entry.ReleaseTag, entry.AssetName, entry.DownloadCount); err != nil {
			return err
		}
	}
	return batch.Send()
}
