package main

import (
	"context"
	stderrors "errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// finderWith builds a finder backed by fake upstreams. The handler receives the
// server name so each one can answer differently.
func finderWith(t *testing.T, store stateStore, names []string, handler func(name string, w http.ResponseWriter)) (*DebugInfoFinder, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	f := NewDebugInfoFinder(store)
	f.servers = map[string]*Server{}
	for _, name := range names {
		name := name
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			handler(name, w)
		}))
		t.Cleanup(srv.Close)
		f.servers[name] = &Server{Name: name, URL: srv.URL, SourceAvailable: 1}
	}
	return f, hits
}

func respondNotFound(name string, w http.ResponseWriter) {
	http.Error(w, "nie ma", http.StatusNotFound)
}

// Headers signed with the server name - they make it detectable if they came from
// a host other than the one returned as the winner.
func respondOKSigned(name string, w http.ResponseWriter) {
	w.Header().Set("x-debuginfod-archive", name)
	w.Header().Set("x-debuginfod-file", name+"/plik.debug")
	w.Header().Set("x-debuginfod-size", "4242")
	w.WriteHeader(http.StatusOK)
}

const testBuildID = "aabbccddeeff00112233445566778899aabbccdd"

// ─── /source rules ──────────────────────────────────────────────────────────

func TestApplySourceRules(t *testing.T) {
	withSource := &Server{Name: "fedora", SourceAvailable: 1}
	noSource := &Server{Name: "debian", SourceAvailable: 0}
	boom := stderrors.New("cos padlo")

	tests := []struct {
		name       string
		endpoint   string
		server     *Server
		err        error
		wantErrIs  error
		wantNilErr bool
	}{
		{"inny endpoint przepuszcza blad", "debuginfo", nil, ErrDebuginfoNotFound, ErrDebuginfoNotFound, false},
		{"inny endpoint przepuszcza sukces", "debuginfo", withSource, nil, nil, true},
		{"source + host bez zrodel", "source", noSource, nil, ErrSourceNotImplemented, false},
		{"source + host ze zrodlami", "source", withSource, nil, nil, true},
		{"source + nie znaleziono", "source", nil, ErrDebuginfoNotFound, ErrSourceNotImplemented, false},
		{"source + inny blad przechodzi", "source", nil, boom, boom, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := applySourceRules(tc.endpoint, tc.server, tc.err)
			if tc.wantNilErr {
				if err != nil {
					t.Fatalf("oczekiwano nil, dostano %v", err)
				}
				return
			}
			if !stderrors.Is(err, tc.wantErrIs) {
				t.Fatalf("oczekiwano %v, dostano %v", tc.wantErrIs, err)
			}
		})
	}
}

// A 404 turned into 501 for /source must keep the original cause, so it stays
// recognisable in the logs.
func TestApplySourceRulesKeepsUnderlyingCause(t *testing.T) {
	_, err := applySourceRules("source", nil, ErrDebuginfoNotFound)
	if !stderrors.Is(err, ErrSourceNotImplemented) {
		t.Error("zgubiono ErrSourceNotImplemented")
	}
	if !stderrors.Is(err, ErrDebuginfoNotFound) {
		t.Error("zgubiono oryginalna przyczyne ErrDebuginfoNotFound")
	}
}

// ─── validation ─────────────────────────────────────────────────────────────

func TestFindByBuildIDRejectsBadLength(t *testing.T) {
	f, hits := finderWith(t, newFakeStore(), []string{"a"}, respondOKSigned)

	for _, buildID := range []string{"", "abc", string(make([]byte, 31)), string(make([]byte, 65))} {
		if _, err := f.FindByBuildID(context.Background(), buildID, "debuginfo"); err == nil {
			t.Errorf("buildID o dlugosci %d powinien byc odrzucony", len(buildID))
		}
	}
	if hits.Load() != 0 {
		t.Errorf("odrzucony buildID poszedl do upstreamow (%d razy)", hits.Load())
	}
}

// ─── fast path ──────────────────────────────────────────────────────────────

// A resolved build ID must generate no upstream traffic at all.
func TestFastPathNoUpstreamTraffic(t *testing.T) {
	store := newFakeStore()
	store.put(BuildIDState{BuildID: testBuildID, LastSuccess: true, LastHost: "fedora"})
	f, hits := finderWith(t, store, []string{"fedora"}, respondOKSigned)

	for i := 0; i < 20; i++ {
		host, err := f.FindByBuildID(context.Background(), testBuildID, "debuginfo")
		if err != nil {
			t.Fatalf("iteracja %d: %v", i, err)
		}
		if host.Name != "fedora" {
			t.Fatalf("iteracja %d: host=%s", i, host.Name)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("szybka sciezka poszla do sieci %d razy", hits.Load())
	}
}

// The /source rules must behave identically on the fast path and the slow one.
func TestFastPathAppliesSourceRules(t *testing.T) {
	store := newFakeStore()
	store.put(BuildIDState{BuildID: testBuildID, LastSuccess: true, LastHost: "debian"})

	f, _ := finderWith(t, store, []string{"debian"}, respondOKSigned)
	f.servers["debian"].SourceAvailable = 0

	if _, err := f.FindByBuildID(context.Background(), testBuildID, "source"); !stderrors.Is(err, ErrSourceNotImplemented) {
		t.Errorf("oczekiwano ErrSourceNotImplemented, dostano %v", err)
	}
	if _, err := f.FindByBuildID(context.Background(), testBuildID, "debuginfo"); err != nil {
		t.Errorf("debuginfo nie powinno byc dotkniete regulami source: %v", err)
	}
}

// A host recorded in the database but no longer in the configuration must not bring the proxy down.
func TestFastPathUnknownHostInState(t *testing.T) {
	store := newFakeStore()
	store.put(BuildIDState{BuildID: testBuildID, LastSuccess: true, LastHost: "hosta-juz-nie-ma"})
	f, hits := finderWith(t, store, []string{"fedora"}, respondOKSigned)

	_, err := f.FindByBuildID(context.Background(), testBuildID, "debuginfo")
	if !stderrors.Is(err, ErrDebuginfoNotFound) {
		t.Errorf("oczekiwano ErrDebuginfoNotFound, dostano %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("nieznany host wywolal ruch do upstreamow")
	}
}

// ─── backoff ────────────────────────────────────────────────────────────────

// The backoff must cut off upstream traffic until the window for that counter expires.
func TestBackoffWindows(t *testing.T) {
	tests := []struct {
		name        string
		counter     uint32
		elapsed     time.Duration
		wantUpstrem bool
	}{
		{"licznik 1, przed oknem 30min", 1, 10 * time.Minute, false},
		{"licznik 1, po oknie 30min", 1, 40 * time.Minute, true},
		{"licznik 2, przed oknem 1h", 2, 40 * time.Minute, false},
		{"licznik 2, po oknie 1h", 2, 90 * time.Minute, true},
		{"licznik 3, przed oknem 2h", 3, 90 * time.Minute, false},
		{"licznik 3, po oknie 2h", 3, 3 * time.Hour, true},
		{"licznik 5, przed oknem 24h", 5, 12 * time.Hour, false},
		{"licznik 5, po oknie 24h", 5, 25 * time.Hour, true},
		{"licznik ponad 30 nigdy", 31, 1000 * time.Hour, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.put(BuildIDState{
				BuildID:   testBuildID,
				Counter:   tc.counter,
				UpdatedAt: time.Now().Add(-tc.elapsed),
			})
			f, hits := finderWith(t, store, []string{"a", "b"}, respondNotFound)

			f.FindByBuildID(context.Background(), testBuildID, "debuginfo")

			if got := hits.Load() > 0; got != tc.wantUpstrem {
				t.Errorf("ruch do upstreamow=%v (trafien %d), oczekiwano %v",
					got, hits.Load(), tc.wantUpstrem)
			}
		})
	}
}

// A failed resolution bumps the counter so the backoff can escalate.
func TestFailedResolutionIncrementsCounter(t *testing.T) {
	store := newFakeStore()
	f, _ := finderWith(t, store, []string{"a", "b"}, respondNotFound)

	if _, err := f.FindByBuildID(context.Background(), testBuildID, "debuginfo"); err == nil {
		t.Fatal("oczekiwano bledu")
	}
	st, ok := store.get(testBuildID)
	if !ok {
		t.Fatal("stan nie zostal zapisany")
	}
	if st.Counter != 1 || st.LastSuccess {
		t.Errorf("stan po porazce: counter=%d last_success=%v", st.Counter, st.LastSuccess)
	}
}

// ─── singleflight ───────────────────────────────────────────────────────────

// N concurrent requests for the same build ID must be ONE resolution.
// Without that, a single popular build ID multiplies outbound traffic by the
// number of clients.
func TestConcurrentResolutionsAreCoalesced(t *testing.T) {
	const upstreams = 6
	names := make([]string, upstreams)
	for i := range names {
		names[i] = string(rune('a' + i))
	}

	release := make(chan struct{})
	store := newFakeStore()
	f, hits := finderWith(t, store, names, func(name string, w http.ResponseWriter) {
		<-release // hold the resolution so every client has time to arrive
		http.Error(w, "nie ma", http.StatusNotFound)
	})

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.FindByBuildID(context.Background(), testBuildID, "debuginfo")
		}()
	}
	// Give the clients time to gather behind the singleflight, then let go.
	waitFor(t, 2*time.Second, func() bool { return hits.Load() > 0 }, "resolucja ruszyla")
	close(release)
	waitGroupDone(t, &wg, 10*time.Second, "klienci nie skonczyli")

	if got := hits.Load(); got != upstreams {
		t.Errorf("25 rownoleglych zapytan dalo %d trafien w upstreamy, oczekiwano %d", got, upstreams)
	}
	if n := store.updateCount(); n != 1 {
		t.Errorf("zapisow stanu: %d, oczekiwano 1", n)
	}
}

// Every waiter gets the same result, not just the leader.
func TestAllWaitersGetSameResult(t *testing.T) {
	release := make(chan struct{})
	f, hits := finderWith(t, newFakeStore(), []string{"fedora"}, func(name string, w http.ResponseWriter) {
		<-release
		respondOKSigned(name, w)
	})

	var mu sync.Mutex
	results := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host, err := f.FindByBuildID(context.Background(), testBuildID, "debuginfo")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results["err"]++
			} else {
				results[host.Name]++
			}
		}()
	}
	waitFor(t, 2*time.Second, func() bool { return hits.Load() > 0 }, "resolucja ruszyla")
	close(release)
	waitGroupDone(t, &wg, 10*time.Second, "klienci nie skonczyli")

	if results["fedora"] != 20 {
		t.Errorf("wyniki: %v, oczekiwano 20x fedora", results)
	}
}

// A client disconnecting must not abort the resolution - otherwise clients that
// disconnect would never warm the cache.
func TestClientCancelStillCompletesResolution(t *testing.T) {
	release := make(chan struct{})
	store := newFakeStore()
	f, _ := finderWith(t, store, []string{"fedora"}, func(name string, w http.ResponseWriter) {
		<-release
		respondOKSigned(name, w)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := f.FindByBuildID(ctx, testBuildID, "debuginfo"); err == nil {
		t.Fatal("oczekiwano bledu z kontekstu")
	}

	close(release)
	waitFor(t, 3*time.Second, func() bool {
		st, ok := store.get(testBuildID)
		return ok && st.LastSuccess
	}, "resolucja dokonczyla sie w tle mimo rozlaczenia klienta")
}

// ─── header consistency ─────────────────────────────────────────────────────

// The headers must come from the host that actually won the race. Kept in a
// variable alongside, they would be overwritten by every server answering 200, and
// the database would receive headers describing a different artifact than
// last_host.
func TestResolvedHeadersMatchWinningHost(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	f, _ := finderWith(t, newFakeStore(), names, respondOKSigned)

	for i := 0; i < 30; i++ {
		host, headers, err := f.tryAllServers(context.Background(), testBuildID, "/buildid/x/debuginfo")
		if err != nil {
			t.Fatalf("runda %d: %v", i, err)
		}
		if headers.Archive != host.Name || headers.File != host.Name+"/plik.debug" {
			t.Fatalf("runda %d: host=%s, ale naglowki od %s", i, host.Name, headers.Archive)
		}
	}
}

// The winning host's headers must end up in the state written to the database.
func TestSuccessfulResolutionPersistsHeaders(t *testing.T) {
	store := newFakeStore()
	f, _ := finderWith(t, store, []string{"fedora"}, respondOKSigned)

	if _, err := f.FindByBuildID(context.Background(), testBuildID, "debuginfo"); err != nil {
		t.Fatal(err)
	}
	st, ok := store.get(testBuildID)
	if !ok {
		t.Fatal("stan nie zapisany")
	}
	if st.LastHost != "fedora" || !st.LastSuccess {
		t.Fatalf("stan: %+v", st)
	}
	if st.ResponseHeaders.Archive != "fedora" || st.ResponseHeaders.Size != 4242 {
		t.Errorf("naglowki nie trafily do stanu: %+v", st.ResponseHeaders)
	}
}

// ─── state cache ────────────────────────────────────────────────────────────

// GetState must hand back a COPY - otherwise a mutation by the caller corrupts the
// LRU entry and the race between the fast path and the background resolution
// returns.
func TestGetStateReturnsCopy(t *testing.T) {
	store := newFakeStore()
	store.put(BuildIDState{BuildID: testBuildID, LastHost: "fedora", Counter: 7})
	f, _ := finderWith(t, store, []string{"fedora"}, respondOKSigned)

	first, err := f.GetState(context.Background(), testBuildID)
	if err != nil {
		t.Fatal(err)
	}
	first.Counter = 999
	first.LastHost = "zepsute"

	second, err := f.GetState(context.Background(), testBuildID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Counter != 7 || second.LastHost != "fedora" {
		t.Errorf("mutacja kopii przeciekla do cache: %+v", second)
	}
}

// Regression: debuginfod.ubuntu.com accepts the connection and sends nothing back.
// Without a limit on the pre-header phase, Fetch hangs until the context deadline -
// and for content fetches that deadline is the server's WriteTimeout, i.e. 60
// minutes.
func TestFetchTimesOutWhenUpstreamNeverSendsHeaders(t *testing.T) {
	// A server that accepts the request and stays silent until the test ends on its own.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	f := NewDebugInfoFinder(newFakeStore())
	tr, ok := f.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport ma typ %T, test nie moze skrocic limitu", f.client.Transport)
	}
	tr.ResponseHeaderTimeout = 150 * time.Millisecond

	// The URL has to name a REMOTE host, because that is the case this guards:
	// the incident was a third-party server, and our own backend is deliberately
	// exempt (see fetchHeaderTimeout). httptest listens on loopback, so the
	// hostname is fake and the dial is pointed back at it - only fetchClientFor's
	// view of the address differs.
	target := strings.TrimPrefix(srv.URL, "http://")
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "silent-upstream.example:") {
			addr = target
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}

	// The context deliberately carries no deadline: if it were the thing killing the
	// request, the test would pass even with ResponseHeaderTimeout removed and would
	// stop guarding anything.
	start := time.Now()
	resp, err := f.Fetch(context.Background(), "http://silent-upstream.example/buildid/aa/debuginfo")
	elapsed := time.Since(start)

	if err == nil {
		resp.Body.Close()
		t.Fatal("Fetch zwrocil odpowiedz, choc serwer nigdy nie wyslal naglowkow")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Fetch przerwal dopiero po %v, limit nie zadzialal", elapsed)
	}
}

// The test above substitutes its own limit, so on its own it would not notice the
// production value being deleted. This one guards that it is set.
func TestProductionTransportHasResponseHeaderTimeout(t *testing.T) {
	f := NewDebugInfoFinder(newFakeStore())
	tr, ok := f.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport ma typ %T, oczekiwano *http.Transport", f.client.Transport)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %v, oczekiwano %v", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	// http.Client.Timeout would also cover reading the body and would cut off large downloads.
	if f.client.Timeout != 0 {
		t.Fatalf("client.Timeout = %v, powinien zostac zerem", f.client.Timeout)
	}
}

// Issue pwndbg/debuginfod.pwndbg.re#1: third-party debuginfod servers need a way to
// attribute traffic to this proxy and to reach us when we cause trouble.
//
// The test guards BOTH paths, because that is exactly where they used to differ:
// content fetches forwarded the client's UA, while probes set nothing and went out
// as "Go-http-client/1.1".
func TestUpstreamRequestsIdentifyThisProxy(t *testing.T) {
	type req struct{ ua, ae string }
	seen := make(chan req, 16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- req{r.Header.Get("user-agent"), r.Header.Get("accept-encoding")}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	f := NewDebugInfoFinder(newFakeStore())
	f.servers = map[string]*Server{"up": {Name: "up", SourceAvailable: 1, URL: up.URL}}

	t.Run("pobranie tresci", func(t *testing.T) {
		// Fetch no longer takes headers from the caller, so forwarding the client's
		// UA is structurally impossible rather than merely omitted.
		resp, err := f.Fetch(context.Background(), up.URL+"/buildid/aa/debuginfo")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got := <-seen
		if got.ua != upstreamUserAgent {
			t.Errorf("upstream zobaczyl UA %q, oczekiwano %q", got.ua, upstreamUserAgent)
		}
		if got.ae != upstreamAcceptEncoding {
			t.Errorf("accept-encoding=%q, oczekiwano %q", got.ae, upstreamAcceptEncoding)
		}
	})

	t.Run("sondowanie", func(t *testing.T) {
		// tryAllServers receives nil instead of headers - this was the forgotten path.
		if _, _, err := f.tryAllServers(context.Background(), "aa", "/buildid/aa/debuginfo"); err != nil {
			t.Fatal(err)
		}
		got := <-seen
		if got.ua != upstreamUserAgent {
			t.Errorf("sonda wyslala UA %q, oczekiwano %q", got.ua, upstreamUserAgent)
		}
		// Probing used to send no accept-encoding at all (nil headers plus
		// DisableCompression on the transport). Both paths are now identical.
		if got.ae != upstreamAcceptEncoding {
			t.Errorf("sonda: accept-encoding=%q, oczekiwano %q", got.ae, upstreamAcceptEncoding)
		}
	})
}
