package main

import (
	"context"
	"crypto/tls"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"
)

var ErrDebuginfoNotFound = errors.New("not found")
var ErrSourceNotImplemented = errors.New("source not implemented")
var ErrDebuginfodTemporaryDown = errors.New("debuginfod temporary down")

type Server struct {
	Name            string
	URL             string
	SourceAvailable int
	Down            bool
}

// stateStore rather than *dbSrv, so the finder tests need no ClickHouse.
// *dbSrv satisfies it implicitly.
type stateStore interface {
	GetState(ctx context.Context, buildID string) (*BuildIDState, error)
	UpdateState(ctx context.Context, state BuildIDState) error
	ResolveLog(ctx context.Context, entries []ResolveLogEntry) error
}

type DebugInfoFinder struct {
	// client probes, and fetches content from third parties. localFetchClient
	// fetches content from our own backend, with no header deadline at all -
	// see fetchHeaderTimeout.
	client           *http.Client
	localFetchClient *http.Client
	servers          map[string]*Server
	// The cache holds VALUES, not pointers - so Get hands back an immutable copy
	// and the fast path in FindByBuildID can read state without synchronisation,
	// even while a resolution for the same build ID is running in the background.
	cache *expirable.LRU[string, BuildIDState]
	db    stateStore

	// sf collapses concurrent resolutions of the same build ID into one run.
	// Without it, N parallel requests for one build ID mean N*len(servers)
	// requests to upstreams.
	sf singleflight.Group
}

const maxResolutionTimeout = 5 * time.Second

// responseHeaderTimeout caps the time from sending the request to receiving the
// response headers. It guards against an upstream that accepts the connection and
// then goes silent without sending anything - which is how debuginfod.ubuntu.com
// behaves: TCP and TLS come up in ~80 ms and then not a single byte arrives.
// Without this limit such a request hangs until the context deadline, and for
// content fetches that deadline is the server's WriteTimeout, i.e. 60 minutes.
//
// It works on HTTP/2 as well ("http2: timeout awaiting response headers"), which
// matters because most upstreams negotiate h2 over ALPN.
//
// 5 s is generous: healthy servers return headers in 0.12-0.7 s. The limit covers
// only the phase before the headers - once bytes are flowing, the context governs.
const responseHeaderTimeout = 5 * time.Second

// fetchHeaderTimeout is the header budget for content fetches from our own
// backend: none.
//
// Not a large number, because any number would be a guess. cmd/nix-debuginfod
// cannot send headers until it has downloaded a NAR and run mkfs.erofs, and how
// long that takes is a property of the closure being built - roughly a minute
// per gigabyte, with no upper bound we control. A limit here does not protect
// against anything: it only decides which artifacts are too large to serve.
//
// Under the probe budget this failed in production twice over. The proxy logged
// "http2: timeout awaiting response headers" at 5 s, and the cancellation
// propagated into the backend and killed mkfs.erofs mid-build ("signal:
// killed") - so the work was not merely abandoned, it had to be redone from
// scratch on every retry, and a large artifact could never finish.
//
// It stays bounded regardless: the request context ends when the client goes
// away, a detached cache download is capped by cacheFetchTimeout, and the
// server's WriteTimeout is 60 minutes. Removing the deadline gives up nothing
// that was actually protecting us.
//
// Third-party fetches keep responseHeaderTimeout. They serve files that already
// exist, so slow headers there mean something is wrong, not that work is in
// progress.
const fetchHeaderTimeout = 0

// isLoopbackAddr reports whether a dial target is this machine.
//
// It decides where TLS verification may be skipped. Making that call from the
// ADDRESS rather than from a per-upstream flag is deliberate: a flag can be set
// on the wrong entry, and the entry that got it would silently stop
// authenticating a third party. An address cannot be got wrong that way - there
// is no way to spell fedora's host such that this returns true.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func NewDebugInfoFinder(db stateStore) *DebugInfoFinder {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	// Two transports, because ResponseHeaderTimeout is a property of the
	// transport rather than of a request, so one client cannot hold both budgets.
	// Everything else about them is identical.
	newTransport := func(headerTimeout time.Duration) *http.Transport {
		return &http.Transport{
			DialContext: dialer.DialContext,
			// The nix backend on loopback serves a certificate it generates
			// at startup and holds only in memory - see cmd/nix-debuginfod/tls.go.
			// Nothing can verify it, and nothing is meant to: it is not
			// establishing identity over a loopback socket, it is the price
			// of TLS, which is there because Go only speaks HTTP/2 over it.
			//
			// Skipping verification for THAT and only that is the point of
			// deciding here rather than with a config field. Every remote
			// upstream keeps a full chain check, and the ServerName is set
			// explicitly so hostname verification still happens for them.
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				cfg := &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
				if isLoopbackAddr(addr) {
					cfg.InsecureSkipVerify = true
				} else if host, _, err := net.SplitHostPort(addr); err == nil {
					cfg.ServerName = host
				}
				return (&tls.Dialer{NetDialer: dialer, Config: cfg}).DialContext(ctx, network, addr)
			},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   3 * time.Second,
			ExpectContinueTimeout: 3 * time.Second,
			ResponseHeaderTimeout: headerTimeout,
			DisableCompression:    true,
		}
	}

	return &DebugInfoFinder{
		db:               db,
		client:           &http.Client{Transport: newTransport(responseHeaderTimeout)},
		localFetchClient: &http.Client{Transport: newTransport(fetchHeaderTimeout)},
		servers: map[string]*Server{
			//{Name: "elfutils", URL: "https://debuginfod.elfutils.org"},  // bugged
			"systemtap":  {Name: "systemtap", Down: false, SourceAvailable: 1, URL: "https://debuginfod.systemtap.org"},
			"opensuse":   {Name: "opensuse", Down: false, SourceAvailable: 1, URL: "https://debuginfod.opensuse.org"},
			"fedora":     {Name: "fedora", Down: false, SourceAvailable: 1, URL: "https://debuginfod.fedoraproject.org"},
			"archlinux":  {Name: "archlinux", Down: false, SourceAvailable: 1, URL: "https://debuginfod.archlinux.org"},
			"artixlinux": {Name: "artixlinux", Down: false, SourceAvailable: 1, URL: "https://debuginfod.artixlinux.org"},
			"cachyos":    {Name: "cachyos", Down: false, SourceAvailable: 1, URL: "https://debuginfod.cachyos.org"}, // untested source
			"centos":     {Name: "centos", Down: false, SourceAvailable: 1, URL: "https://debuginfod.centos.org"},
			"debian":     {Name: "debian", Down: false, SourceAvailable: 0, URL: "https://debuginfod.debian.net"},
			"ubuntu":     {Name: "ubuntu", Down: true, SourceAvailable: 0, URL: "https://debuginfod.ubuntu.com"},
			//"alpine":     {Name: "alpine", Down: false, SourceAvailable: 0, URL: "https://debuginfod.achill.org"}, // offline
			// https, and on 8034: cmd/nix-debuginfod serves TLS so that HTTP/2 -
			// and with it 103 Early Hints - is available on the connection this
			// proxy keeps open across many build IDs. cmd/nix-nar-old is the
			// plain-HTTP one on 8032.
			"nix": {Name: "nix", Down: false, SourceAvailable: 1, URL: "https://127.0.0.1:8034"},
		},
		cache: expirable.NewLRU[string, BuildIDState](10000, nil, time.Hour*24),
	}
}

// upstreamUserAgent identifies US to third-party debuginfod servers and gives
// them an address to reach when we cause trouble.
//
// There used to be two behaviours here and both were wrong. Content fetches
// forwarded the CLIENT's user-agent, so the upstream saw somebody's gdb and had
// no way to attribute the traffic to this proxy. Probing (bgFindByBuildID calling
// tryAllServers with nil) set nothing at all, so the default
// "Go-http-client/1.1" went out - and probing is what generates 10x more traffic
// than fetching, because every unknown build ID means a parallel request to every
// upstream at once. The traffic most burdensome to other people's servers was
// therefore completely anonymous, and their only response to trouble would be an
// IP ban.
//
// As a side effect we stop leaking the client's UA, which told nine third-party
// servers the distribution and OS version of a specific pwndbg user.
const upstreamUserAgent = "pwndbg-debuginfod (+https://github.com/pwndbg/debuginfod.pwndbg.re)"

// newUpstreamRequest builds every request this proxy makes upstream, and takes
// no headers from the caller.
//
// Both Fetch and Probe go through it. That is the whole point: the last time
// probing had a request-building path of its own it forgot the user-agent, and
// probing is the traffic that matters most to other people's servers - one
// unknown build ID means a parallel request to every upstream at once. Client
// headers deliberately do not travel further.
func (f *DebugInfoFinder) newUpstreamRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("user-agent", upstreamUserAgent)
	req.Header.Set("accept-encoding", upstreamAcceptEncoding)
	return req, nil
}

// errProbeSatisfied aborts a probe from inside the 1xx callback. Returning an
// error from Got1xxResponse is the documented way to stop a request, and it
// stops it *without* waiting for the final headers - which for our own nix
// backend arrive only after the whole erofs image has been built.
// fetchClientFor picks the transport for a content fetch. The choice is made
// from the address, not from a flag on the upstream entry: a flag can be set on
// the wrong line, and that line would then quietly wait forever on a third
// party. There is no way to spell fedora's host such that this returns the
// deadline-free client.
func (f *DebugInfoFinder) fetchClientFor(rawURL string) *http.Client {
	u, err := url.Parse(rawURL)
	if err != nil {
		return f.client
	}
	host := u.Hostname()
	if host == "localhost" {
		return f.localFetchClient
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return f.localFetchClient
	}
	return f.client
}

var errProbeSatisfied = errors.New("probe answered by early hints")

// Probe asks an upstream whether it has a build ID. It wants the answer, not the
// bytes.
//
// The distinction is not cosmetic. Resolution runs under maxResolutionTimeout,
// and the transport gives up on headers after responseHeaderTimeout - measured:
// a 103 does NOT defer either. cmd/nix-debuginfod cannot send final headers
// until it has downloaded a NAR and run mkfs.erofs, tens of seconds, so on the
// Fetch path it loses every race and gets recorded as "does not have it".
//
// It does send a 103 carrying x-status: 200 as soon as the build ID resolves,
// which is the answer a probe is after. Only that header counts: any upstream
// might send Early Hints for its own reasons, and treating those as "has it"
// would poison buildid_state with a host that never had the file.
//
// Headers from a 103 are partial by nature - at that moment nix knows the
// archive but not the size, because the file does not exist yet.
func (f *DebugInfoFinder) Probe(ctx context.Context, url string) (StateHeaders, error) {
	req, err := f.newUpstreamRequest(ctx, url)
	if err != nil {
		return StateHeaders{}, err
	}

	var (
		early    StateHeaders
		gotEarly bool
	)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code != http.StatusEarlyHints || h.Get("X-Status") != strconv.Itoa(http.StatusOK) {
				return nil
			}
			early = StateHeaders{
				Size:         strToUInt64(h.Get("X-Debuginfod-Size")),
				File:         h.Get("X-Debuginfod-File"),
				Archive:      h.Get("X-Debuginfod-Archive"),
				ImaSignature: h.Get("X-Debuginfod-Imasignature"),
			}
			gotEarly = true
			return errProbeSatisfied
		},
	}))

	resp, err := f.client.Do(req)
	if gotEarly {
		if resp != nil {
			resp.Body.Close()
		}
		// The request was aborted on purpose; there is no response to close.
		return early, nil
	}
	if err != nil {
		return StateHeaders{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.ProtoMajor == 1 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		}
		if resp.StatusCode == http.StatusNotFound {
			return StateHeaders{}, ErrDebuginfoNotFound
		} else if resp.StatusCode == http.StatusNotImplemented {
			return StateHeaders{}, ErrSourceNotImplemented
		}
		return StateHeaders{}, fmt.Errorf("invalid status code %d", resp.StatusCode)
	}
	return StateHeaders{
		Size:         strToUInt64(resp.Header.Get("x-debuginfod-size")),
		File:         resp.Header.Get("x-debuginfod-file"),
		Archive:      resp.Header.Get("x-debuginfod-archive"),
		ImaSignature: resp.Header.Get("x-debuginfod-imasignature"),
	}, nil
}

func (f *DebugInfoFinder) Fetch(ctx context.Context, url string) (*http.Response, error) {
	req, err := f.newUpstreamRequest(ctx, url)
	if err != nil {
		return nil, err
	}

	resp, err := f.fetchClientFor(url).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.ProtoMajor == 1 {
			// Drain (up to 4 KiB) so the HTTP/1.x connection can be reused;
			// unread data at Close forces the Transport to drop the connection.
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) // 4kb
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrDebuginfoNotFound
		} else if resp.StatusCode == http.StatusNotImplemented {
			return nil, ErrSourceNotImplemented
		}
		return nil, fmt.Errorf("invalid status code %d", resp.StatusCode)
	}
	return resp, nil
}

// resolveResult ties the winning server to ITS headers. The headers must travel
// through the channel together with the server - kept in a variable alongside, they
// would be overwritten by every server that managed to answer 200, so the database
// would end up with headers from a different host than the one in last_host.
type resolveResult struct {
	server  *Server
	headers StateHeaders
}

func (f *DebugInfoFinder) tryAllServers(ctx context.Context, buildID string, requestURI string) (*Server, StateHeaders, error) {
	logger := log.WithField("build_id", buildID).WithField("func", "tryAllServers")

	ctxForFind, cancelFind := context.WithCancel(ctx)
	defer cancelFind()

	ch := make(chan resolveResult, 1)
	var errCounter atomic.Int64
	maxErrors := int64(len(f.servers))

	var entries []ResolveLogEntry
	entriesMu := sync.Mutex{}

	for _, server := range f.servers {
		if server.Down {
			continue
		}

		go func(server *Server) {
			startAt := time.Now()
			headers, err := f.Probe(ctxForFind, server.URL+requestURI)

			// log
			{
				entry := ResolveLogEntry{
					Timestamp:    startAt,
					BuildID:      buildID,
					ResolvedHost: server.Name,
					Duration:     time.Since(startAt),
					Success:      err == nil,
					ErrorMsg:     errToString(err),
				}
				entriesMu.Lock()
				entries = append(entries, entry)
				entriesMu.Unlock()
			}

			if err != nil {
				logger.WithError(err).WithField("host", server.Name).Info("fetching server err")
				if errCounter.Add(1) == maxErrors {
					close(ch)
				}
				return
			}
			logger.WithField("host", server.Name).Info("fetching server ok")

			select {
			case ch <- resolveResult{server: server, headers: headers}:
				cancelFind()
			default:
			}
		}(server)
	}

	select {
	case res, ok := <-ch:
		// The losing goroutines may still be appending to entries, so we read a copy
		// under the mutex - otherwise this is a race on the slice header.

		// TODO: move this to the background
		entriesMu.Lock()
		resolveEntries := make([]ResolveLogEntry, len(entries))
		copy(resolveEntries, entries)
		entriesMu.Unlock()

		if err := f.db.ResolveLog(context.Background(), resolveEntries); err != nil {
			logger.WithError(err).Error("ResolveLog err")
		}
		if !ok {
			return nil, StateHeaders{}, ErrDebuginfoNotFound
		}
		return res.server, res.headers, nil
	}
}

// GetState returns a COPY of the state - the caller may mutate it freely, because
// it is not shared with the LRU or with a resolution running in the background.
func (f *DebugInfoFinder) GetState(ctx context.Context, buildID string) (BuildIDState, error) {
	// 1. Fetch from cache
	// 2. or Fetch from db + add to cache
	if state, ok := f.cache.Get(buildID); ok {
		return state, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	state, err := f.db.GetState(ctx, buildID)
	cancel()

	if errors.Is(err, ErrDbNoRow) {
		fresh := BuildIDState{BuildID: buildID}
		f.cache.Add(buildID, fresh)
		return fresh, nil
	} else if err != nil {
		return BuildIDState{}, err
	}

	f.cache.Add(buildID, *state)
	return *state, nil
}

func (f *DebugInfoFinder) UpdateState(ctx context.Context, state BuildIDState) error {
	f.cache.Add(state.BuildID, state)
	if err := f.db.UpdateState(ctx, state); err != nil {
		return err
	}
	return nil
}

func (f *DebugInfoFinder) FindByBuildID(ctx context.Context, buildID string, endpointName string) (*Server, error) {
	if len(buildID) > 64 {
		return nil, fmt.Errorf("buildID too long %d", len(buildID))
	}
	if len(buildID) < 32 {
		return nil, fmt.Errorf("buildID too short %d", len(buildID))
	}

	state, err := f.GetState(ctx, buildID)
	if err != nil {
		return nil, err
	}
	if state.LastSuccess {
		host, ok := f.servers[state.LastHost]
		if !ok {
			log.WithField("build_id", buildID).WithField("host", state.LastHost).
				Warn("FindByBuildID found host in db, but not in code")
			return applySourceRules(endpointName, nil, ErrDebuginfoNotFound)
		}
		if host.Down {
			return nil, stderrors.Join(fmt.Errorf("host %s is down", host.Name), ErrDebuginfodTemporaryDown)
		}
		return applySourceRules(endpointName, host, nil)
	}

	// Slow path: the upstreams have to be asked.
	// DoChan rather than Do: when the client disconnects we want to leave through
	// ctx.Done() but let the resolution finish in the background and record its
	// state (the same thing the chRes goroutine used to do).
	chRes := f.sf.DoChan(buildID, func() (any, error) {
		return f.bgFindByBuildID(buildID)
	})

	select {
	case res := <-chRes:
		server, _ := res.Val.(*Server)
		return applySourceRules(endpointName, server, res.Err)
	case <-ctx.Done():
		// timeout, return normal 500 error

		return nil, ctx.Err()
	}
}

// applySourceRules - special case for source handling, for better caching by buildid.
// Shared by the fast and slow paths so the two cannot drift apart later.
func applySourceRules(endpointName string, server *Server, err error) (*Server, error) {
	if endpointName != "source" {
		return server, err
	}
	if stderrors.Is(err, ErrDebuginfoNotFound) {
		return server, stderrors.Join(ErrSourceNotImplemented, err)
	}
	if err == nil && server.SourceAvailable == 0 {
		return server, ErrSourceNotImplemented
	}
	return server, err
}

// bgFindByBuildID MUST be called only through f.sf.DoChan from FindByBuildID -
// otherwise the stampede on upstreams returns for concurrent requests for the same
// build ID.
//
// The state is read here AGAIN even though FindByBuildID already did so: while
// waiting in the singleflight queue another leader may have overtaken us and
// resolved this build ID already.
func (f *DebugInfoFinder) bgFindByBuildID(buildID string) (*Server, error) {
	logger := log.WithField("build_id", buildID).WithField("func", "FindByBuildID")

	state, err := f.GetState(context.Background(), buildID)
	if err != nil {
		return nil, err
	}
	if state.LastSuccess {
		host, ok := f.servers[state.LastHost]
		if !ok {
			logger.WithField("host", state.LastHost).Warn("FindByBuildID found host in db, but not in code")
			return nil, ErrDebuginfoNotFound
		}
		if host.Down {
			return nil, stderrors.Join(fmt.Errorf("host %s is down", host.Name), ErrDebuginfodTemporaryDown)
		}
		return host, nil
	}

	if state.Counter > 30 {
		logger.WithField("counter", state.Counter).Info("FindByBuildID skip find, counter to big")
		return nil, ErrDebuginfoNotFound
	}

	elapsed := time.Since(state.UpdatedAt)
	switch state.Counter {
	case 1:
		if elapsed < 30*time.Minute {
			return nil, ErrDebuginfoNotFound
		}
	case 2:
		if elapsed < 1*time.Hour {
			return nil, ErrDebuginfoNotFound
		}
	case 3:
		if elapsed < 2*time.Hour {
			return nil, ErrDebuginfoNotFound
		}
	default:
		if elapsed < 24*time.Hour {
			return nil, ErrDebuginfoNotFound
		}
	}

	defer func() {
		// no context needed here, one with a 5s deadline is set below
		if err := f.UpdateState(context.Background(), state); err != nil {
			logger.WithError(err).Error("UpdateState err")
		}
	}()

	state.UpdatedAt = time.Now()

	ctxResolution, cancelResolution := context.WithTimeout(context.Background(), maxResolutionTimeout)
	host, stateHeaders, err := f.tryAllServers(ctxResolution, buildID, "/buildid/"+buildID+"/debuginfo")
	cancelResolution()

	state.LastSuccess = err == nil
	if err != nil {
		state.LastError = err.Error()
		state.Counter += 1
		return nil, err
	} else {
		state.LastHost = host.Name
		// Headers from the /debuginfo probe - they describe the debuginfo artifact, not executable/source.
		state.ResponseHeaders = stateHeaders
		return host, nil
	}
}
