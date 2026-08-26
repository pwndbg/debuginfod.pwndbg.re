package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/julienschmidt/httprouter"
	"golang.org/x/crypto/acme/autocert"
)

// projectURL is where anything that is not a debuginfod route gets sent.
const projectURL = "https://github.com/pwndbg/debuginfod.pwndbg.re/"

type serverSrv struct {
	db     *dbSrv
	finder *DebugInfoFinder
	cache  *fileCache      // nil = file cache disabled
	stats  *statsCollector // nil = /stats disabled

	server   *http.Server
	autocert *autocert.Manager
}

func (s *serverSrv) InitRouter() {
	if Config.ListenPort == 443 {
		s.autocert = &autocert.Manager{
			Cache:      autocert.DirCache(Config.CertCachePath),
			Prompt:     autocert.AcceptTOS,
			Email:      Config.LetsencryptEmail,
			HostPolicy: autocert.HostWhitelist(Config.Domains...),
		}
	}

	router := httprouter.New()
	router.GET("/buildid/:buildid/executable", AccessLogMiddleware(s.db, "executable", s.proxyRequest))
	router.GET("/buildid/:buildid/debuginfo", AccessLogMiddleware(s.db, "debuginfo", s.proxyRequest))
	router.GET("/buildid/:buildid/source/*path", AccessLogMiddleware(s.db, "source", s.proxyRequest))
	router.GET("/status", func(writer http.ResponseWriter, request *http.Request, params httprouter.Params) {
		writer.WriteHeader(http.StatusOK)
	})

	if s.stats != nil {
		// Deliberately not wrapped in AccessLogMiddleware: the page renders
		// access_log, so logging its own hits would fold page views into the
		// numbers it presents.
		router.GET("/stats", s.stats.Handler)
	}

	// Anything that is not a registered route - "/" included - goes to the project
	// page. Deliberately outside AccessLogMiddleware: these are not debuginfod
	// requests, so logging them would put crawler and browser traffic into
	// access_log and into every chart on /stats.
	//
	// 302 rather than 301: a permanent redirect is cached by browsers and is
	// awkward to take back if a real page ever lands here. The edge does not cache
	// it either way - the Cache Rule covers 200/404/501 only.
	router.NotFound = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, projectURL, http.StatusFound)
	})

	server := &http.Server{
		Addr:         Config.ListenIP + ":" + strconv.Itoa(Config.ListenPort),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Minute, // downloading big files
	}
	if s.autocert != nil {
		server.TLSConfig = &tls.Config{GetCertificate: s.autocert.GetCertificate}
	}
	s.server = server
}

func (s *serverSrv) ListenAndServe() error {
	if s.autocert != nil {
		go func() {
			if err := http.ListenAndServe(Config.ListenIP+":80", s.autocert.HTTPHandler(nil)); err != nil {
				log.WithError(err).Fatal("Error starting http autocert server")
			}
		}()
	}
	if s.server.TLSConfig != nil {
		return s.server.ListenAndServeTLS("", "")
	}
	return s.server.ListenAndServe()
}

func (s *serverSrv) proxyRequest(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	ctx := r.Context()
	entry := CtxGetAccessLogEntry(ctx)

	buildId := ps.ByName("buildid")
	// todo: x-debuginfod-max-time header???

	host, err := s.finder.FindByBuildID(ctx, buildId, entry.EndpointName)
	if err != nil {
		return err
	}
	entry.ResolvedHost = host.Name

	fetch := func(ctx context.Context) (*http.Response, error) {
		return s.finder.Fetch(ctx, host.URL+r.URL.EscapedPath())
	}

	if s.cache != nil && cacheableEndpoints[entry.EndpointName] {
		key := cacheKey(entry.EndpointName, buildId, ps.ByName("path"))
		err := s.cache.Serve(ctx, key, w, host.Name, fetch)
		// ErrCacheBusy means no download slot was free. Nothing has been sent
		// yet, so fall through to the uncached path rather than returning 500
		// for a request we would have served fine before the cache existed.
		if !errors.Is(err, ErrCacheBusy) {
			return err
		}
		w.Header().Set("x-cache", "OVERLOADED")
	}

	resp, err := fetch(ctx)
	if err != nil {
		return err
	}

	for _, respHeader := range proxyHeaders {
		w.Header().Del(respHeader)
		if val := resp.Header.Get(respHeader); len(val) > 0 {
			w.Header().Set(respHeader, val)
		}
	}
	w.Header().Set("x-server", host.Name)
	if w.Header().Get("x-cache") == "" {
		w.Header().Set("x-cache", "BYPASS")
	}
	w.WriteHeader(resp.StatusCode)

	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		return fmt.Errorf("copying response body: %w", err)
	}
	return nil
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

	s := serverSrv{
		db:     db,
		finder: NewDebugInfoFinder(db),
	}

	if Config.CacheEnabled {
		cache, err := newFileCache(Config.CachePath, Config.CacheMaxBytes)
		if err != nil {
			log.WithError(err).Fatal("initializing file cache")
		}
		s.cache = cache
		go cache.EvictLoop(ctx)
		go NewCacheStatCollector(Config.CachePath, db).Worker(ctx, Config.CacheStatsInterval)
		log.WithField("path", Config.CachePath).
			WithField("max_bytes", Config.CacheMaxBytes).Info("file cache enabled")
	}

	if Config.GHRangesEnabled {
		go NewGHRangeCollector(Config.GHRangesServices).Worker(ctx, Config.GHRangesInterval)
		log.WithField("services", Config.GHRangesServices).
			WithField("interval", Config.GHRangesInterval).Info("GitHub IP range tagging enabled")
	}

	if Config.StatsEnabled {
		s.stats = NewStatsCollector(db, Config.StatsDays)
		go s.stats.Worker(ctx, Config.StatsInterval)
		log.WithField("days", Config.StatsDays).
			WithField("interval", Config.StatsInterval).Info("/stats enabled")
	}

	s.InitRouter()

	log.Infof("Starting HTTP server on %s:%d\n", Config.ListenIP, Config.ListenPort)
	if err := s.ListenAndServe(); err != nil {
		log.WithError(err).Fatal("starting/stoping server")
	}
}
