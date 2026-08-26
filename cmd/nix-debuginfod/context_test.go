package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/julienschmidt/httprouter"

	"github.com/pwndbg/debuginfod.pwndbg.re/nix"
)

type nullLogger struct{}

func (nullLogger) AccessLog(context.Context, AccessLogEntry) error { return nil }

// captureLogger keeps the entry the middleware produced so a test can assert on
// what would have been stored.
type captureLogger struct{ into *AccessLogEntry }

func (c *captureLogger) AccessLog(_ context.Context, e AccessLogEntry) error {
	*c.into = e
	return nil
}

// A handler that fails before writing anything gets a real status.
func TestErrorBeforeBodyGetsAStatus(t *testing.T) {
	h := AccessLogMiddleware(nullLogger{}, "debuginfo",
		func(http.ResponseWriter, *http.Request, httprouter.Params) error {
			return errors.New("boom")
		})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/buildid/x/debuginfo", nil), nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rec.Code)
	}
}

// A handler that fails AFTER the first byte must abort the connection rather than
// append an error page to a partial artifact. Serving 200 + truncated bytes +
// "Internal Server Error" produces a self-consistent response holding corrupt
// debuginfo that the client cannot detect.
func TestErrorAfterBodyAbortsConnection(t *testing.T) {
	h := AccessLogMiddleware(nullLogger{}, "debuginfo",
		func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) error {
			w.Write([]byte("partial-debuginfo"))
			return errors.New("upstream died mid-stream")
		})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic; a truncated body was finished off as if it were complete")
		}
		if r != http.ErrAbortHandler {
			t.Fatalf("panic = %v, want http.ErrAbortHandler", r)
		}
	}()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/buildid/x/debuginfo", nil), nil)
}

// duration_100kb_ms exists to be size-independent: duration_ms grows with the
// file, so it cannot be compared between a 20 KB and a 40 MB response. The
// sample must therefore fire only once the fixed threshold is actually crossed,
// and stay zero otherwise - a zero means "never got that far", not "was instant".
func TestThroughputSampleOnlyPastTheThreshold(t *testing.T) {
	run := func(body int) AccessLogEntry {
		t.Helper()
		var got AccessLogEntry
		db := &captureLogger{into: &got}
		h := AccessLogMiddleware(db, "debuginfo",
			func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) error {
				chunk := make([]byte, 4096)
				for sent := 0; sent < body; sent += len(chunk) {
					if _, err := w.Write(chunk); err != nil {
						return err
					}
				}
				return nil
			})
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/buildid/x/debuginfo", nil), nil)
		return got
	}

	small := run(throughputSampleBytes / 2)
	if small.Duration100kb != 0 {
		t.Errorf("a response below the threshold reported %v; want 0", small.Duration100kb)
	}

	big := run(throughputSampleBytes * 3)
	if big.Duration100kb == 0 {
		t.Error("a response past the threshold reported 0; the sample never fired")
	}
	if big.Duration100kb > big.Duration {
		t.Errorf("sample %v exceeds total %v - it must be taken partway through",
			big.Duration100kb, big.Duration)
	}
}

// The constant defines what the stored column means. cmd/proxy writes the same
// column name into access_log, and the two tables are meant to be queried the
// same way, so a divergence here would silently make the numbers incomparable.
func TestThroughputSampleMatchesProxy(t *testing.T) {
	if throughputSampleBytes != 100<<10 {
		t.Errorf("throughputSampleBytes = %d; cmd/proxy uses 100 KiB and both write duration_100kb_ms",
			throughputSampleBytes)
	}
}

// A source path that is not in the tree is a miss, not a fault. cmd/proxy stores
// a 404 as a cacheable negative result with backoff and Cloudflare caches it for
// two hours, while 5xx is cached nowhere - so answering 500 for a missing file
// turns one bad guess into unbounded repeat traffic.
func TestMissingFileIs404Not500(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"nix not found", nix.ErrNixDebuginfoNotFound, http.StatusNotFound},
		{"fs.ErrNotExist", fs.ErrNotExist, http.StatusNotFound},
		{"wrapped fs.ErrNotExist", fmt.Errorf("/build/x.c: %w", fs.ErrNotExist), http.StatusNotFound},
		{"anything else", errors.New("upstream exploded"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := AccessLogMiddleware(nullLogger{}, "source",
				func(http.ResponseWriter, *http.Request, httprouter.Params) error { return tc.err })
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodGet, "/buildid/x/source/y", nil), nil)
			if rec.Code != tc.want {
				t.Errorf("status %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// The status has to be read after the error page is written, not before. Getting
// this backwards logged 0 for every failed request - so the log was blank for
// exactly the requests worth investigating, and no 404 was countable at all.
func TestFailedRequestsAreLoggedWithTheirStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want uint16
	}{
		{"missing", fs.ErrNotExist, http.StatusNotFound},
		{"broken", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got AccessLogEntry
			h := AccessLogMiddleware(&captureLogger{into: &got}, "source",
				func(http.ResponseWriter, *http.Request, httprouter.Params) error { return tc.err })
			h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/buildid/x/source/y", nil), nil)

			if got.Status != tc.want {
				t.Errorf("logged status %d, want %d", got.Status, tc.want)
			}
			if got.ErrorMsg == "" {
				t.Error("logged no error message")
			}
		})
	}
}
