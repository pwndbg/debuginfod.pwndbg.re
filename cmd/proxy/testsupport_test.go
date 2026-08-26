package main

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Tests must not reach api.cloudflare.com. GetCFIPManager hides its instance
// behind a sync.Once, so we get there first with our own empty one - IsTrusted
// then returns false and getRealIP uses RemoteAddr, which is exactly what the
// tests want.
func init() {
	cfipMgrInstanceOnce.Do(func() { cfipMgrInstance = &CFIPManager{} })
}

// ─── stateStore double ──────────────────────────────────────────────────────

type fakeStore struct {
	mu       sync.Mutex
	states   map[string]BuildIDState
	updates  int
	resolves int
	getErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{states: map[string]BuildIDState{}}
}

func (s *fakeStore) GetState(ctx context.Context, buildID string) (*BuildIDState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	st, ok := s.states[buildID]
	if !ok {
		return nil, ErrDbNoRow
	}
	cp := st
	return &cp, nil
}

func (s *fakeStore) UpdateState(ctx context.Context, state BuildIDState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.BuildID] = state
	s.updates++
	return nil
}

func (s *fakeStore) ResolveLog(ctx context.Context, entries []ResolveLogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolves++
	return nil
}

func (s *fakeStore) get(buildID string) (BuildIDState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[buildID]
	return st, ok
}

func (s *fakeStore) put(state BuildIDState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.BuildID] = state
}

func (s *fakeStore) updateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates
}

// ─── accessLogger double ────────────────────────────────────────────────────

type fakeAccessLog struct {
	mu      sync.Mutex
	entries []AccessLogEntry
}

func (f *fakeAccessLog) AccessLog(ctx context.Context, entry AccessLogEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entry)
	return nil
}

func (f *fakeAccessLog) last(t *testing.T) AccessLogEntry {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		t.Fatal("access log pusty")
	}
	return f.entries[len(f.entries)-1]
}

// ─── gated ResponseWriter ───────────────────────────────────────────────────

// gateWriter is a deterministic "slow client": the first Write blocks until the
// test closes the gate. That way we never compare timings (which is flaky) and
// instead check whether the others can finish while one stands still.
type gateWriter struct {
	mu     sync.Mutex
	hdr    http.Header
	status int
	buf    bytes.Buffer

	gate   chan struct{}
	passed bool
}

func newGateWriter(gate chan struct{}) *gateWriter {
	return &gateWriter{hdr: http.Header{}, gate: gate}
}

func (g *gateWriter) Header() http.Header { return g.hdr }

func (g *gateWriter) WriteHeader(code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status = code
}

func (g *gateWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	needGate := g.gate != nil && !g.passed
	if needGate {
		g.passed = true
	}
	g.mu.Unlock()

	if needGate {
		<-g.gate
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *gateWriter) bytesWritten() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Len()
}

func (g *gateWriter) body() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]byte(nil), g.buf.Bytes()...)
}

// ─── helpers ────────────────────────────────────────────────────────────────

// waitFor spins until the condition holds; fails the test after timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout czekajac na: %s", msg)
}

// waitGroupDone waits on a WaitGroup with a deadline instead of hanging the test.
func waitGroupDone(t *testing.T, wg *sync.WaitGroup, timeout time.Duration, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("timeout: %s", msg)
	}
}
