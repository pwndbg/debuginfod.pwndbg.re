package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/julienschmidt/httprouter"

	"github.com/pwndbg/debuginfod.pwndbg.re/nix"
)

// throughputSampleBytes is the threshold past which we sample how long the
// response took to deliver. A fixed sample size makes the result comparable
// between requests for files of different sizes - which `duration_ms` cannot be,
// because it grows with size, and debuginfo here runs to tens of megabytes.
//
// It must stay identical to cmd/proxy's constant. The two services write the
// same column name into two tables meant to be queried the same way; a different
// threshold would silently make those numbers mean different things.
const throughputSampleBytes = 100 << 10 // 100 KiB

type loggingResponseWriter struct {
	http.ResponseWriter

	statusCode  int
	wroteHeader bool
	sentHints   bool
	bytesSent   uint64
	startAt     time.Time
	// sampleDuration is the time from the start of the request to the moment
	// throughputSampleBytes have been written. -1 until the threshold is crossed,
	// which is how "never got that far" stays distinguishable from "took no time".
	sampleDuration time.Duration
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	// 1xx is informational: it goes out now but does NOT commit the response, so
	// a later 404 or 500 is still possible and the access log must not record
	// 103 as the status. Without this branch the first Early Hints would have
	// sealed every response as "103" and silenced the real status.
	if code >= 100 && code < 200 {
		if !lrw.wroteHeader {
			lrw.ResponseWriter.WriteHeader(code)
			lrw.sentHints = true
		}
		return
	}
	if lrw.wroteHeader {
		// Already committed - e.g. io.Copy failed partway through the body.
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
	n, err := lrw.ResponseWriter.Write(b)
	lrw.bytesSent += uint64(n)
	if lrw.sampleDuration == -1 && lrw.bytesSent > throughputSampleBytes {
		lrw.sampleDuration = time.Since(lrw.startAt)
	}
	return n, err
}

type ctxKeyAccessLog struct{}

type HandleWithErr func(http.ResponseWriter, *http.Request, httprouter.Params) error

// accessLogger rather than *dbSrv, so the middleware tests need no ClickHouse.
type accessLogger interface {
	AccessLog(ctx context.Context, entry AccessLogEntry) error
}

func AccessLogMiddleware(db accessLogger, endpointName string, next HandleWithErr) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, startAt: start, sampleDuration: -1}

		ip, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			log.WithError(err).WithField("ip", r.RemoteAddr).Error("Error parsing remote address")
		}

		entry := &AccessLogEntry{
			Timestamp:    time.Now(),
			EndpointName: endpointName,
			RemoteIP:     ip.Addr(),
			Method:       r.Method,
			RequestURI:   r.RequestURI,
			UserAgent:    r.Header.Get("User-Agent"),
			BuildID:      ps.ByName("buildid"),
		}
		ctx := context.WithValue(r.Context(), ctxKeyAccessLog{}, entry)
		r = r.WithContext(ctx)

		respErr := next(lrw, r, ps)

		// Captured before anything below writes an error page: http.Error commits
		// the response itself, after which "the handler sent something" can no
		// longer be told from "we sent the error".
		committedByHandler := lrw.wroteHeader

		if respErr != nil && !committedByHandler {
			// fs.ErrNotExist as well as the nix one: a source path that is simply
			// not in the tree is a miss, not a fault. The distinction is not
			// cosmetic - cmd/proxy records a 404 as a cacheable negative result
			// with backoff, and Cloudflare caches 404 for two hours and does not
			// cache 5xx at all. Returning 500 for a missing file therefore turns
			// one bad guess into unbounded repeat traffic.
			switch {
			case errors.Is(respErr, nix.ErrNixDebuginfoNotFound), errors.Is(respErr, fs.ErrNotExist):
				http.Error(lrw, "not found", http.StatusNotFound)
			default:
				http.Error(lrw, "Internal Server Error", http.StatusInternalServerError)
			}
		}

		// Only now is the entry built: http.Error above is what sets the status,
		// so reading lrw.statusCode any earlier recorded 0 for every failed
		// request - i.e. exactly the ones worth looking at afterwards, and every
		// 404 would have been invisible in the log.
		if respErr != nil {
			entry.ErrorMsg = respErr.Error()
		}
		entry.Duration = time.Since(start)
		if lrw.sampleDuration != -1 {
			entry.Duration100kb = lrw.sampleDuration
		}
		entry.Status = uint16(lrw.statusCode)
		entry.BytesSent = lrw.bytesSent
		entry.ResponseHeaders = StateHeaders{
			Size:         strToUInt64(lrw.Header().Get("x-debuginfod-size")),
			File:         lrw.Header().Get("x-debuginfod-file"),
			Archive:      lrw.Header().Get("x-debuginfod-archive"),
			ImaSignature: lrw.Header().Get("x-debuginfod-imasignature"),
		}

		log.WithError(respErr).Printf("%s - %s %s %d %s",
			entry.RemoteIP,
			entry.Method,
			entry.RequestURI,
			entry.Status,
			entry.Duration,
		)

		if err := db.AccessLog(context.Background(), *entry); err != nil {
			log.WithError(err).Error("failed to write access log")
		}

		// A failure AFTER the first byte went out. The status cannot be changed
		// any more, so the only thing separating a truncated response from a
		// complete one is how the connection ends. Without this, net/http finishes
		// the body normally - with a valid chunked terminator - and the client
		// stores a syntactically perfect 200 that is missing bytes, i.e. corrupt
		// debuginfo it has no way to detect.
		//
		// This is the same failure cmd/nix-nar-old's
		// TestErrorAfterHeadersBreaksConnection pins. It was harmless here only
		// while the handler never wrote anything.
		//
		// ErrAbortHandler drops the connection without logging a panic. The access
		// log has already been written above.
		if respErr != nil && committedByHandler {
			panic(http.ErrAbortHandler)
		}
	}
}

func CtxGetAccessLogEntry(ctx context.Context) *AccessLogEntry {
	v := ctx.Value(ctxKeyAccessLog{})
	if v == nil {
		log.Fatal("access log entry is missing")
	}
	return v.(*AccessLogEntry)
}

func strToUInt64(s string) uint64 {
	if len(s) == 0 {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
