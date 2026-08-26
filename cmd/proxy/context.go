package main

import (
	"context"
	stderrors "errors"
	"net/http"
	"strconv"
	"time"

	"github.com/julienschmidt/httprouter"
)

// throughputSampleBytes is the threshold past which we sample how long the response
// took to deliver. A fixed sample size makes the result comparable between requests for
// files of different sizes - which `duration_ms` cannot be, because it grows with size.
//
// Changing this value changes the meaning of the `duration_100kb_ms` column in ClickHouse:
// old data stops being comparable with new, so the column must be renamed if it changes.
const throughputSampleBytes = 100 << 10 // 100 KiB

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	bytesSent   uint64
	startAt     time.Time
	// sampleDuration is the time from the start of the request to the moment
	// throughputSampleBytes have been written. -1 until the threshold is crossed.
	sampleDuration time.Duration
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	if lrw.wroteHeader {
		// response already committed, e.g. when io.Copy fails mid-body
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
// *dbSrv satisfies it implicitly.
type accessLogger interface {
	AccessLog(ctx context.Context, entry AccessLogEntry) error
}

func AccessLogMiddleware(db accessLogger, endpointName string, next HandleWithErr) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, startAt: start, sampleDuration: -1}

		ip, err := getRealIP(r)
		if err != nil {
			log.WithError(err).WithField("ip", r.RemoteAddr).Error("Error parsing remote address")
		}

		entry := &AccessLogEntry{
			Timestamp:    time.Now(),
			EndpointName: endpointName,
			RemoteIP:     ip,
			Method:       r.Method,
			RequestURI:   r.RequestURI,
			UserAgent:    r.Header.Get("User-Agent"),
			BuildID:      ps.ByName("buildid"),
		}
		ctx := context.WithValue(r.Context(), ctxKeyAccessLog{}, entry)
		r = r.WithContext(ctx)

		respErr := next(lrw, r, ps)

		// Captured BEFORE the error is mapped: http.Error below writes headers
		// itself, so afterwards lrw.wroteHeader is true for every 404/501/500 and
		// "the handler sent something" can no longer be told apart from "we sent
		// the error".
		committedByHandler := lrw.wroteHeader

		if respErr != nil && !committedByHandler {
			// only if nothing has reached the client yet; otherwise the status is already sent
			switch {
			case stderrors.Is(respErr, ErrSourceNotImplemented):
				http.Error(lrw, "source not implemented", http.StatusNotImplemented)
			case stderrors.Is(respErr, ErrDebuginfoNotFound):
				http.Error(lrw, "not found", http.StatusNotFound)
			case stderrors.Is(respErr, ErrDebuginfodTemporaryDown):
				http.Error(lrw, respErr.Error(), http.StatusServiceUnavailable)
			default:
				http.Error(lrw, "Internal Server Error", http.StatusInternalServerError)
			}
		}

		// access log after response
		{
			if respErr != nil {
				entry.ErrorMsg = respErr.Error()
			}
			entry.Duration = time.Since(start)
			if lrw.sampleDuration != -1 {
				entry.Duration100kb = lrw.sampleDuration
			}
			entry.Status = uint16(lrw.statusCode)
			entry.CacheStatus = lrw.Header().Get("x-cache")
			entry.BytesSent = lrw.bytesSent
			entry.ResponseHeaders = StateHeaders{
				Size:         strToUInt64(lrw.Header().Get("x-debuginfod-size")),
				File:         lrw.Header().Get("x-debuginfod-file"),
				Archive:      lrw.Header().Get("x-debuginfod-archive"),
				ImaSignature: lrw.Header().Get("x-debuginfod-imasignature"),
			}

			if err := db.AccessLog(context.Background(), *entry); err != nil {
				log.WithError(err).Error("failed to write access log")
			}
		}

		// An error AFTER the headers went out: the status can no longer be changed,
		// so the only thing separating a truncated response from a complete one is
		// how the connection is closed. Without this net/http finishes the body
		// normally (and with chunked encoding appends a valid terminator), so the
		// client sees a syntactically complete 200 with missing bytes and stores
		// corrupt debuginfo as if it were good.
		//
		// ErrAbortHandler drops the connection without logging a panic - it is the
		// documented way to say "abort, the body is incomplete". The access log has
		// already been written above.
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
