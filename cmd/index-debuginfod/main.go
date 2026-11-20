// Command index-debuginfod serves debuginfo and executables for build ids that
// cmd/indexer has recorded: libc6-dbg from Ubuntu, Pythons from
// python-build-standalone. No distribution debuginfod has either.
//
// It holds nothing of its own. A request looks the build id up in
// indexed_buildids, fetches the archive named there, streams it up to the
// member and streams the member out. cmd/proxy caches the result like any
// other upstream's, so each artifact is extracted about once.
//
//	GET /{source}/buildid/{id}/debuginfo
//	GET /{source}/buildid/{id}/executable
//
// The source prefix is what lets cmd/proxy list each source as a separate
// upstream - "ubuntu-dbg" and "uv-python" on /stats, and a SourceVia for the
// Ubuntu one only - while one process serves both.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/pwndbg/debuginfod.pwndbg.re/useragent"
)

func main() {
	applyLogLevel(envOr("LOG_LEVEL", "info"))
	addr := envOr("LISTEN_ADDR", "127.0.0.1:8039")
	dsn := envOr("CLICKHOUSE_DSN", "clickhouse://127.0.0.1:9000?username=cypis&password=cypis")

	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		log.WithError(err).Fatal("parsing CLICKHOUSE_DSN")
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		log.WithError(err).Fatal("opening ClickHouse")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := conn.Ping(ctx); err != nil {
		log.WithError(err).Warn("ClickHouse not reachable yet; lookups will fail until it is")
	}
	cancel()

	s := newServer(&chIndex{conn: conn},
		// No overall timeout: a python asset is ~126 MB and the member may sit
		// at the end of it. The request context bounds the transfer instead.
		useragent.Client(&http.Client{}, "index"))

	server := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		// Matches cmd/proxy's: a large artifact on a slow mirror is minutes.
		WriteTimeout: 60 * time.Minute,
	}
	log.WithField("addr", addr).Info("index-debuginfod starting")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.WithError(err).Fatal("server failed")
	}
}

func newServer(idx index, hc *http.Client) *srv {
	return &srv{
		idx:    idx,
		hc:     hc,
		lookup: expirable.NewLRU[string, []entry](lookupCacheSize, nil, lookupCacheTTL),
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
