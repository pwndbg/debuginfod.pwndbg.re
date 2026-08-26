package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A probe wants the answer, not the bytes. cmd/nix-debuginfod cannot send final
// headers until it has downloaded a NAR and run mkfs.erofs, so an upstream that
// only answers at the end loses every resolution race - it is the 103 that
// carries the answer in time.
func TestProbeAnswersFromEarlyHints(t *testing.T) {
	const buildTime = 3 * time.Second
	served := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-status", "200")
		w.Header().Set("x-debuginfod-archive", "nar/abc.nar.xz")
		w.WriteHeader(http.StatusEarlyHints)
		select {
		case <-time.After(buildTime):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("BYTES"))
		close(served)
	}))
	defer srv.Close()

	f := NewDebugInfoFinder(nil)
	start := time.Now()
	headers, err := f.Probe(context.Background(), srv.URL+"/buildid/x/debuginfo")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if took > buildTime/2 {
		t.Errorf("took %s; the whole point is to answer before the build finishes (%s)", took, buildTime)
	}
	// Partial by nature: at 103 time the file does not exist, so there is no size.
	if headers.Archive != "nar/abc.nar.xz" {
		t.Errorf("archive = %q, want it carried on the 103", headers.Archive)
	}
}

// Any upstream may send Early Hints for its own reasons. Treating those as
// "has it" would record a host in buildid_state that never had the file.
func TestProbeIgnoresEarlyHintsWithoutOurMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("link", "</style.css>; rel=preload") // a plain, unrelated 103
		w.WriteHeader(http.StatusEarlyHints)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	f := NewDebugInfoFinder(nil)
	if _, err := f.Probe(context.Background(), srv.URL+"/buildid/x/debuginfo"); err != ErrDebuginfoNotFound {
		t.Errorf("err = %v, want ErrDebuginfoNotFound - an unrelated 103 must not count as a hit", err)
	}
}

// Without a 103 the probe behaves exactly as before: the final status decides.
func TestProbeFallsBackToTheFinalResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"found", http.StatusOK, nil},
		{"missing", http.StatusNotFound, ErrDebuginfoNotFound},
		{"no sources", http.StatusNotImplemented, ErrSourceNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("x-debuginfod-size", "1234")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			f := NewDebugInfoFinder(nil)
			h, err := f.Probe(context.Background(), srv.URL+"/buildid/x/debuginfo")
			if err != tc.want {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && h.Size != 1234 {
				t.Errorf("size = %d, want 1234 from the final headers", h.Size)
			}
		})
	}
}

// The reason this method exists separately: probing used to have its own
// request path and forgot the user-agent, so the traffic most burdensome to
// third parties was anonymous.
func TestProbeSendsTheUpstreamUserAgent(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("user-agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewDebugInfoFinder(nil)
	if _, err := f.Probe(context.Background(), srv.URL+"/buildid/x/debuginfo"); err != nil {
		t.Fatal(err)
	}
	if ua := <-got; ua != upstreamUserAgent {
		t.Errorf("user-agent = %q, want %q", ua, upstreamUserAgent)
	}
}

// Our own backend gets no header deadline, third parties keep the probe budget.
// The choice is made from the address, so it cannot be handed to a third party
// by editing the wrong upstream entry.
func TestOnlyOurBackendFetchesWithoutAHeaderDeadline(t *testing.T) {
	f := NewDebugInfoFinder(nil)
	budget := func(c *http.Client) time.Duration {
		return c.Transport.(*http.Transport).ResponseHeaderTimeout
	}

	if got := budget(f.client); got != responseHeaderTimeout {
		t.Errorf("probe budget = %s, want %s", got, responseHeaderTimeout)
	}
	if got := budget(f.localFetchClient); got != 0 {
		t.Errorf("local fetch budget = %s, want no deadline", got)
	}

	for _, tc := range []struct {
		url   string
		local bool
	}{
		{"https://127.0.0.1:8034/buildid/x/debuginfo", true},
		{"http://localhost:8034/buildid/x/debuginfo", true},
		{"https://[::1]:8034/buildid/x/debuginfo", true},
		{"https://debuginfod.fedoraproject.org/buildid/x/debuginfo", false},
		{"https://127.0.0.1.evil.com/buildid/x/debuginfo", false},
		{"https://localhost.evil.com/buildid/x/debuginfo", false},
		{"://broken", false},
	} {
		got := f.fetchClientFor(tc.url) == f.localFetchClient
		if got != tc.local {
			t.Errorf("fetchClientFor(%q) local = %v, want %v", tc.url, got, tc.local)
		}
	}
}

// The production failure: an upstream that needs longer than the probe budget to
// produce headers. Under one shared client the proxy gave up at 5 s and the
// cancellation killed mkfs.erofs mid-build.
//
// httptest listens on 127.0.0.1, so this takes the deadline-free path.
func TestFetchToleratesHeadersSlowerThanTheProbeBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(responseHeaderTimeout + time.Second):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte("BYTES"))
	}))
	defer srv.Close()

	f := NewDebugInfoFinder(nil)
	resp, err := f.Fetch(context.Background(), srv.URL+"/buildid/x/debuginfo")
	if err != nil {
		t.Fatalf("fetch from our own backend cut off after the probe budget: %v", err)
	}
	resp.Body.Close()
}

// The probe budget is unchanged, and a probe is what keeps a slow upstream from
// holding up resolution. Bounded by an explicit context so a regression here
// fails the test rather than hanging it.
func TestProbeStillGivesUpOnSlowUpstreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	f := NewDebugInfoFinder(nil)
	ctx, cancel := context.WithTimeout(context.Background(), responseHeaderTimeout+3*time.Second)
	defer cancel()

	start := time.Now()
	if _, err := f.Probe(ctx, srv.URL+"/buildid/x/debuginfo"); err == nil {
		t.Fatal("probe waited for a silent upstream")
	}
	if took := time.Since(start); took > responseHeaderTimeout+2*time.Second {
		t.Errorf("probe gave up after %s, want about %s", took.Round(time.Millisecond), responseHeaderTimeout)
	}
}
