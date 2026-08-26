package main

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/julienschmidt/httprouter"
)

// safeBuf - the server logs from its own goroutine, so the buffer must be safe.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type middlewareResult struct {
	status    int
	body      string
	entry     AccessLogEntry
	serverLog string
	// transportErr != nil means the connection was dropped. The middleware does
	// that on purpose when a handler fails AFTER the headers went out - it is the
	// only way for the client to tell a truncated response from a complete one.
	transportErr error
}

// runMiddleware pushes one request through AccessLogMiddleware on a REAL net/http
// server. httptest.NewRecorder is not enough here: only a real server emits the
// "superfluous response.WriteHeader" warning.
func runMiddleware(t *testing.T, endpointName string, h HandleWithErr) middlewareResult {
	t.Helper()

	fake := &fakeAccessLog{}
	handler := AccessLogMiddleware(fake, endpointName, h)

	var logBuf safeBuf
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, httprouter.Params{httprouter.Param{Key: "buildid", Value: testBuildID}})
	}))
	srv.Config.ErrorLog = stdlog.New(&logBuf, "", 0)
	srv.Start()

	var (
		status   int
		body     []byte
		transErr error
	)
	resp, err := http.Get(srv.URL + "/buildid/" + testBuildID + "/debuginfo")
	if err != nil {
		transErr = err
	} else {
		status = resp.StatusCode
		var readErr error
		body, readErr = io.ReadAll(resp.Body)
		resp.Body.Close()
		transErr = readErr
	}

	// Close waits for handling to finish, so the server log and access log are complete.
	srv.Close()

	return middlewareResult{
		status:       status,
		body:         string(body),
		entry:        fake.last(t),
		serverLog:    logBuf.String(),
		transportErr: transErr,
	}
}

// When a handler commits the response and only then fails (e.g. io.Copy breaks
// mid-stream), the middleware must not try to overwrite the status.
// Symptoms before the fix: "superfluous response.WriteHeader" in the logs AND
// - worse - "Internal Server Error" appended into the middle of a debuginfo file.
func TestMiddlewareNoSuperfluousWriteHeaderAfterCommit(t *testing.T) {
	res := runMiddleware(t, "debuginfo", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("czesc-pliku"))
		return fmt.Errorf("copying response body: %w", io.ErrUnexpectedEOF)
	})

	if strings.Contains(res.serverLog, "superfluous response.WriteHeader") {
		t.Errorf("serwer zalogowal superfluous WriteHeader:\n%s", res.serverLog)
	}
	if strings.Contains(res.body, "Internal Server Error") {
		t.Errorf("tresc odpowiedzi zanieczyszczona komunikatem bledu: %q", res.body)
	}
	// The handler failed AFTER committing, so the status cannot be taken back - all
	// that remains is dropping the connection so the client does not mistake partial
	// content for complete. See TestTruncatedResponseIsDetectableByClient.
	if res.transportErr == nil {
		t.Error("polaczenie zamkniete czysto - klient nie odrozni uciecia od kompletu")
	}
	// The error must reach the access log even though the client got a 200.
	if res.entry.ErrorMsg == "" {
		t.Error("blad nie zostal odnotowany w access logu")
	}
}

// When a handler fails BEFORE writing anything, the middleware maps the error to
// a status. That status must also land in the access log - previously http.Error
// wrote to the raw ResponseWriter, bypassing the counter, and status=0 went to the
// database.
func TestMiddlewareMapsErrorsAndLogsStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"nie znaleziono", ErrDebuginfoNotFound, http.StatusNotFound},
		{"zrodla niezaimplementowane", ErrSourceNotImplemented, http.StatusNotImplemented},
		{"opakowany 501", stderrors.Join(ErrSourceNotImplemented, ErrDebuginfoNotFound), http.StatusNotImplemented},
		{"nieznany blad", stderrors.New("cos padlo"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runMiddleware(t, "debuginfo", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
				return tc.err
			})

			if res.status != tc.wantStatus {
				t.Errorf("klient dostal status %d, oczekiwano %d", res.status, tc.wantStatus)
			}
			if int(res.entry.Status) != tc.wantStatus {
				t.Errorf("access log zapisal status %d, oczekiwano %d", res.entry.Status, tc.wantStatus)
			}
			if res.entry.BytesSent == 0 {
				t.Error("access log nie policzyl bajtow tresci bledu")
			}
			if strings.Contains(res.serverLog, "superfluous") {
				t.Errorf("nieoczekiwany log serwera:\n%s", res.serverLog)
			}
		})
	}
}

// A successful request records the status, the size and the endpoint name.
func TestMiddlewareLogsSuccessfulRequest(t *testing.T) {
	res := runMiddleware(t, "executable", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
		w.Header().Set("x-debuginfod-size", "1234")
		w.Write([]byte("dane"))
		return nil
	})

	if res.entry.Status != http.StatusOK {
		t.Errorf("status=%d, oczekiwano 200 (niejawne przy pierwszym Write)", res.entry.Status)
	}
	if res.entry.BytesSent != 4 {
		t.Errorf("bytes_sent=%d, oczekiwano 4", res.entry.BytesSent)
	}
	if res.entry.EndpointName != "executable" {
		t.Errorf("endpoint=%q", res.entry.EndpointName)
	}
	if res.entry.BuildID != testBuildID {
		t.Errorf("buildid=%q", res.entry.BuildID)
	}
	if res.entry.ResponseHeaders.Size != 1234 {
		t.Errorf("naglowki debuginfod nie trafily do logu: %+v", res.entry.ResponseHeaders)
	}
}

// The duration_100kb sample must measure the time to deliver the first 100 KiB and
// NOT the whole response - that is the point, so requests of different sizes stay
// comparable.
func TestMiddlewareThroughputSampleExcludesSlowTail(t *testing.T) {
	const tail = 80 * time.Millisecond

	res := runMiddleware(t, "debuginfo", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
		chunk := make([]byte, throughputSampleBytes/2)
		w.Write(chunk)
		w.Write(chunk)
		w.Write(chunk) // this is where the threshold is crossed
		time.Sleep(tail)
		w.Write(chunk)
		return nil
	})

	if res.entry.Duration100kb <= 0 {
		t.Fatalf("probka nie zostala pobrana mimo przekroczenia progu (duration_100kb=%v)",
			res.entry.Duration100kb)
	}
	if res.entry.Duration100kb >= res.entry.Duration {
		t.Errorf("probka (%v) nie jest mniejsza niz calosc (%v) - wolny ogon nie zostal wykluczony",
			res.entry.Duration100kb, res.entry.Duration)
	}
	if res.entry.Duration < tail {
		t.Errorf("calkowity czas %v krotszy niz wymuszona pauza %v", res.entry.Duration, tail)
	}
}

// A response smaller than the threshold leaves 0 = "not applicable". Putting the
// total duration there would poison the averages, because the time of a small 404
// is not comparable to the time to deliver 100 KiB of debuginfo.
func TestMiddlewareThroughputSampleZeroBelowThreshold(t *testing.T) {
	res := runMiddleware(t, "debuginfo", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
		w.Write(make([]byte, 1024))
		return nil
	})

	if res.entry.Duration100kb != 0 {
		t.Errorf("duration_100kb=%v, oczekiwano 0 dla odpowiedzi ponizej progu",
			res.entry.Duration100kb)
	}
	if res.entry.Duration <= 0 {
		t.Error("czas calkowity powinien byc mierzony zawsze")
	}
}

// A 404 must not take a sample either - it is the most common case on this service.
func TestMiddlewareThroughputSampleZeroOnError(t *testing.T) {
	res := runMiddleware(t, "debuginfo", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
		return ErrDebuginfoNotFound
	})

	if res.entry.Duration100kb != 0 {
		t.Errorf("duration_100kb=%v dla 404, oczekiwano 0", res.entry.Duration100kb)
	}
}

func TestStrToUInt64(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
	}{
		{"", 0},
		{"0", 0},
		{"4538432", 4538432},
		{"nie-liczba", 0},
		{"-5", 0},
		{"99999999999999999999999", 0}, // overflow
	}
	for _, tc := range tests {
		if got := strToUInt64(tc.in); got != tc.want {
			t.Errorf("strToUInt64(%q)=%d, oczekiwano %d", tc.in, got, tc.want)
		}
	}
}

// x-cache has to reach access_log, otherwise the hit rate exists only on the wire
// and cannot be computed after the fact.
func TestMiddlewareRecordsCacheStatus(t *testing.T) {
	for _, want := range []string{"HIT", "MISS", "COALESCED", "BYPASS", "OVERLOADED"} {
		t.Run(want, func(t *testing.T) {
			res := runMiddleware(t, "debuginfo", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
				w.Header().Set("x-cache", want)
				w.Write([]byte("dane"))
				return nil
			})
			if res.entry.CacheStatus != want {
				t.Errorf("access_log cache_status=%q, oczekiwano %q", res.entry.CacheStatus, want)
			}
		})
	}
}

// A request that never reached the cache (e.g. a 404 from the resolver) leaves the
// field empty - not an invented status.
func TestMiddlewareCacheStatusEmptyWhenCacheNotReached(t *testing.T) {
	res := runMiddleware(t, "debuginfo", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
		return ErrDebuginfoNotFound
	})
	if res.entry.CacheStatus != "" {
		t.Errorf("cache_status=%q, oczekiwano pustego", res.entry.CacheStatus)
	}
}
