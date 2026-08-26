package main

import (
	"context"
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

// Init creates the one table this service owns.
//
// nix_access_log has never been deployed, so everything below is still the
// initial schema and needs no migration. From the first deploy onwards that
// stops being true: CREATE TABLE IF NOT EXISTS is a no-op on a table that exists,
// so it cannot add a column to one. Any column added after that point needs its
// own `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` beside the CREATE, or deployed
// instances keep the old table and the first INSERT fails at runtime with "No
// such column" - which is exactly how apparent_bytes broke cache_stats.
func (s *dbSrv) Init(ctx context.Context) error {
	if err := s.conn.Exec(ctx, `
CREATE TABLE IF NOT EXISTS nix_access_log (
    timestamp     DateTime DEFAULT now(),  -- czas obsługi requesta
    remote_ip     IPv6,                  -- IP klienta
    method        LowCardinality(String),                  -- GET / POST / etc.
    endpoint_name        LowCardinality(String),                  -- GET / POST / etc.
    request_uri   String,                  -- ścieżka żądania (np. /buildid/abc123/executable)
    status        UInt16,                  -- kod odpowiedzi (np. 200, 404, 500)
    user_agent    String,                  -- nagłówek User-Agent
    duration_ms   UInt32,                  -- czas obsługi requesta w milisekundach
    duration_100kb_ms UInt32,              -- czas dostarczenia pierwszych 100 KiB (0 = nie doszło do progu)
    bytes_sent    UInt64,                  -- liczba bajtów wysłanych do klienta
    buildid       String,                  -- opcjonalnie: wyciągnięty z URI buildid (jeśli jest)
    error_msg     String,                   -- opis błędu, jeśli wystąpił
    response_headers        Tuple(size UInt64, file String, archive String, imasignature String),
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY (timestamp, request_uri);
`); err != nil {
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
	ErrorMsg        string
	ResponseHeaders StateHeaders
}

func (s *dbSrv) AccessLog(ctx context.Context, entry AccessLogEntry) error {
	ctx = clickhouse.Context(ctx, clickhouse.WithStdAsync(false))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	const query = `
		INSERT INTO nix_access_log
		(timestamp, remote_ip, method, endpoint_name, request_uri, status, user_agent, duration_ms, duration_100kb_ms, bytes_sent, buildid, error_msg, response_headers)
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
