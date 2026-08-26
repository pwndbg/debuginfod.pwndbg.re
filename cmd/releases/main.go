// Command releases serves releases.pwndbg.re. It redirects
// /releases/<version>/<file> to the matching GitHub asset, logs each redirect to
// releases_access_log, polls the GitHub API hourly for asset download counts into
// github_download_stats, and renders both onto /stats.
//
// Both used to live in cmd/proxy. They share nothing with the debuginfod path -
// no cache, no upstream resolution, no buildid_state - and this service answers
// on its own hostname, which cloudflared routes to this port.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
)

const (
	listenAddr = "127.0.0.1:8033"

	// serveHost is the only Host this service answers for. cloudflared already
	// routes by hostname, so this is a second line rather than the first: it keeps
	// the service inert if something else is ever pointed at this port.
	serveHost = "releases.pwndbg.re"

	// projectURL takes anything that is not a release URL.
	projectURL = "https://github.com/pwndbg/pwndbg/releases/"

	releaseURLFormat = "https://github.com/pwndbg/pwndbg/releases/download/%s/%s"
)

// hostMatches compares against serveHost with any port removed. r.Host carries a
// port for direct requests (curl to 127.0.0.1:8033) but not through the tunnel.
func hostMatches(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(host, serveHost)
}

func redirectRelease(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if !hostMatches(r.Host) {
		http.NotFound(w, r)
		return
	}
	target := fmt.Sprintf(releaseURLFormat, ps.ByName("version"), ps.ByName("file"))
	http.Redirect(w, r, target, http.StatusFound)
}

func newRouter(db accessLogger, stats *statsCollector) *httprouter.Router {
	router := httprouter.New()
	router.GET("/releases/:version/:file", AccessLogMiddleware(db, redirectRelease))
	if stats != nil {
		// Outside AccessLogMiddleware: the page renders releases_access_log, so
		// logging its own hits would fold page views into the numbers it shows.
		router.GET("/stats", stats.Handler)
	}
	router.NotFound = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, projectURL, http.StatusFound)
	})
	return router
}

func main() {
	if err := ParseConfig(); err != nil {
		log.WithError(err).Fatal("parsing config")
	}
	applyLogLevel(Config.LogLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := NewDB(ctx, Config.ClickhouseDSN)
	if err != nil {
		log.WithError(err).Fatal("connecting db")
	}
	if err := db.Init(ctx); err != nil {
		log.WithError(err).Fatal("initializing db")
	}

	go NewGhCollector(db).Worker(ctx)

	var stats *statsCollector
	if Config.StatsEnabled {
		stats = NewStatsCollector(db, Config.StatsDays)
		go stats.Worker(ctx, Config.StatsInterval)
		log.WithField("days", Config.StatsDays).
			WithField("interval", Config.StatsInterval).Info("/stats enabled")
	}

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           newRouter(db, stats),
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.WithField("addr", listenAddr).WithField("host", serveHost).Info("releases starting")
	if err := srv.ListenAndServe(); err != nil {
		log.WithError(err).Fatal("server failed")
	}
}
