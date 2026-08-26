package main

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"github.com/julienschmidt/httprouter"
)

// loggingResponseWriter exists only to capture the status code: the redirect body
// is a fixed stub, so its length says nothing worth storing.
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	if lrw.wroteHeader {
		return
	}
	lrw.wroteHeader = true
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func (lrw *loggingResponseWriter) Write(b []byte) (int, error) {
	if !lrw.wroteHeader {
		lrw.WriteHeader(http.StatusOK)
	}
	return lrw.ResponseWriter.Write(b)
}

// accessLogger rather than *dbSrv, so the middleware tests need no ClickHouse.
// *dbSrv satisfies it implicitly.
type accessLogger interface {
	AccessLog(ctx context.Context, entry AccessLogEntry) error
}

// clientIP returns the address to record.
//
// cmd/proxy validates RemoteAddr against Cloudflare's published ranges before
// trusting CF-Connecting-IP; here that list could never match anything, because
// this service listens on loopback and is reached only through cloudflared on the
// same host - so the tunnel always connects from 127.0.0.1. cmd/proxy carries an
// explicit loopback carve-out for exactly that case (cfip.go IsTrusted), which is
// the only branch that would ever fire for us. Reproducing the fetch-and-refresh
// machinery would add a background worker and an outbound HTTP dependency to
// decide something already decided.
//
// The loopback check is still worth making rather than trusting the header
// unconditionally: it keeps the header from being believed if this ever listens on
// a routable address. The trade is the one cmd/proxy already accepts - any process
// on this host can set the header, and --network host makes that the whole machine.
func clientIP(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	remote := ap.Addr()
	if hdr := r.Header.Get("CF-Connecting-IP"); hdr != "" && remote.IsLoopback() {
		if ip, err := netip.ParseAddr(hdr); err == nil {
			return ip
		}
	}
	return remote
}

// AccessLogMiddleware records one row per request into releases_access_log.
//
// It is applied to the release redirect only, never to the catch-all NotFound
// handler: that one answers every stray path a crawler invents, and folding it in
// would put that traffic into every number on /stats.
func AccessLogMiddleware(db accessLogger, next httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w}

		next(lrw, r, ps)

		entry := AccessLogEntry{
			Timestamp: start,
			RemoteIP:  clientIP(r),
			Country:   r.Header.Get("CF-IPCountry"),
			// httprouter has already matched and split the path, so these are the
			// exact segments the redirect was built from - no re-parsing, and no
			// query string to strip.
			Version:    ps.ByName("version"),
			File:       ps.ByName("file"),
			RequestURI: r.RequestURI,
			Status:     uint16(lrw.statusCode),
			UserAgent:  r.Header.Get("User-Agent"),
		}

		// context.Background(): by now the request context is usually cancelled,
		// and a cancelled insert would drop the row we came here to write.
		if err := db.AccessLog(context.Background(), entry); err != nil {
			log.WithError(err).Error("failed to write access log")
		}
	}
}
