package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/julienschmidt/httprouter"
)

// When a download breaks AFTER the headers went out, the client has to notice.
//
// The status can no longer be taken back - the 200 is gone. All that is left is
// how the connection is closed: a normal close (with a valid terminator under
// chunked encoding) hands the client a syntactically complete response with
// missing bytes, and debuginfod will store that file locally as valid and serve
// it from its own cache for the rest of the entry's life.
func TestTruncatedResponseIsDetectableByClient(t *testing.T) {
	const sent = 4096

	fake := &fakeAccessLog{}
	handler := AccessLogMiddleware(fake, "debuginfo",
		func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
			// Mirrors fileCache.writeBody: headers and part of the body reach the
			// client, and only then does the source return an error.
			w.Header().Set("content-type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write(make([]byte, sent)); err != nil {
				return err
			}
			return io.ErrUnexpectedEOF
		})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, httprouter.Params{httprouter.Param{Key: "buildid", Value: testBuildID}})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/buildid/" + testBuildID + "/debuginfo")
	if err != nil {
		t.Fatalf("zapytanie: %v", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)

	t.Logf("klient: status=%d przeczytano=%d bajtow readErr=%v", resp.StatusCode, len(body), readErr)

	// This is the entire point of the test: the transport MUST report an error. If
	// the response ends cleanly, the client has no signal that the file is short.
	if readErr == nil {
		t.Errorf("klient odczytal %d bajtow BEZ bledu - ucieta odpowiedz wyglada na kompletna", len(body))
	}

	// The error must also be recorded, so this can be diagnosed after the fact.
	entry := fake.last(t)
	if entry.ErrorMsg == "" {
		t.Error("access_log nie zapisal przyczyny urwania")
	}
	if entry.BytesSent != sent {
		t.Errorf("access_log: bytes_sent=%d, oczekiwano %d", entry.BytesSent, sent)
	}
	t.Logf("access_log: status=%d bytes_sent=%d error_msg=%q",
		entry.Status, entry.BytesSent, entry.ErrorMsg)
}

// An error BEFORE the headers go out must still map to a status rather than drop
// the connection - otherwise ordinary 404s would turn into transport errors.
func TestErrorBeforeHeadersStillMapsToStatus(t *testing.T) {
	fake := &fakeAccessLog{}
	handler := AccessLogMiddleware(fake, "debuginfo",
		func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
			return ErrDebuginfoNotFound
		})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, httprouter.Params{httprouter.Param{Key: "buildid", Value: testBuildID}})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/buildid/" + testBuildID + "/debuginfo")
	if err != nil {
		t.Fatalf("zapytanie: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("404 nie powinno zrywac polaczenia: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, oczekiwano 404", resp.StatusCode)
	}
}
