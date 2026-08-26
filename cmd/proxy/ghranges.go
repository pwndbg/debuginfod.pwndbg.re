package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"
)

// githubMetaURL publishes the address ranges GitHub's own services speak from.
const githubMetaURL = "https://api.github.com/meta"

// ghPrefix is one CIDR together with the meta key it was listed under.
type ghPrefix struct {
	Prefix  string
	Service string
}

// GHRangeCollector keeps the in-process range set current.
//
// Why this exists: /stats is meant to describe people using the service, and
// CI runners are not people. A workflow that installs pwndbg on every push
// makes a handful of build IDs look like sustained demand and inflates the
// distinct-client count, and it does so from address space that changes
// underneath us - the actions ranges are Azure allocations and there are
// several thousand of them, churning often enough that a list checked in here
// would be wrong within weeks.
type GHRangeCollector struct {
	services []string
	hc       *http.Client
}

func NewGHRangeCollector(services []string) *GHRangeCollector {
	return &GHRangeCollector{
		services: services,
		hc:       &http.Client{Timeout: 30 * time.Second},
	}
}

// retryUntilLoaded is how often the first fetch is retried.
//
// The ranges used to be mirrored into ClickHouse, and a dictionary there could
// classify anything logged before the process had them. That is gone, so the
// in-process set is now the only classifier and every request that arrives
// before it loads is recorded as unclassified for good. Retrying on the normal
// interval would mean a day of that if GitHub happens to be unreachable at
// startup - and with the overwhelming majority of traffic being CI, a day of
// unclassified rows badly distorts the page. Seconds of it does not.
const retryUntilLoaded = 15 * time.Second

// Worker refreshes on startup and then every interval.
//
// It refreshes immediately rather than waiting out the first tick, and keeps
// retrying quickly until it has succeeded once, because until then nothing can
// be classified at all.
func (c *GHRangeCollector) Worker(ctx context.Context, interval time.Duration) {
	for {
		err := c.Refresh(ctx)
		if err != nil && ctx.Err() == nil {
			log.WithError(err).Warn("refreshing GitHub IP ranges failed")
		}

		wait := interval
		if ghTagSet.Load() == nil {
			// Nothing has ever loaded, so requests are being logged
			// unclassified right now. Come back soon.
			wait = retryUntilLoaded
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (c *GHRangeCollector) Refresh(ctx context.Context) error {
	prefixes, err := c.fetch(ctx)
	if err != nil {
		return err
	}
	// A failed fetch leaves the previous set in place rather than clearing it:
	// stale ranges classify almost everything correctly, an empty set nothing.
	ghTagSet.Store(newIPTagSet(prefixes))
	log.WithField("prefixes", len(prefixes)).
		WithField("services", c.services).Info("GitHub IP ranges refreshed")
	return nil
}

func (c *GHRangeCollector) fetch(ctx context.Context) ([]ghPrefix, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubMetaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("user-agent", upstreamUserAgent)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", githubMetaURL, resp.Status)
	}
	return parseGitHubMeta(resp.Body, c.services)
}

// parseGitHubMeta pulls the requested service keys out of the meta document.
//
// Decoding into json.RawMessage rather than a struct keeps the parser from
// caring about the two dozen other keys - ssh_keys, commit_signing_keys and the
// rest are arrays of strings that are not CIDRs at all - and lets an unknown
// service name be reported instead of silently yielding nothing.
func parseGitHubMeta(r io.Reader, services []string) ([]ghPrefix, error) {
	var doc map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", githubMetaURL, err)
	}

	var out []ghPrefix
	for _, service := range services {
		raw, ok := doc[service]
		if !ok {
			return nil, fmt.Errorf("%s has no %q key", githubMetaURL, service)
		}
		var cidrs []string
		if err := json.Unmarshal(raw, &cidrs); err != nil {
			return nil, fmt.Errorf("%s: %q is not a list of CIDRs: %w", githubMetaURL, service, err)
		}

		skipped := 0
		for _, cidr := range cidrs {
			// Validated here rather than left to ClickHouse. These strings are
			// the dictionary's key, and one malformed entry fails the whole
			// dictionary load - which would turn a single bad line at GitHub's
			// end into no filtering at all, reported nowhere.
			if _, err := netip.ParsePrefix(cidr); err != nil {
				skipped++
				continue
			}
			out = append(out, ghPrefix{Prefix: cidr, Service: service})
		}
		if skipped > 0 {
			log.WithField("service", service).WithField("skipped", skipped).
				Warn("dropped unparseable CIDRs from the GitHub meta document")
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%s yielded no usable prefixes for %v", githubMetaURL, services)
	}
	return out, nil
}

// ipTagSet answers "which GitHub service owns this address" for the several
// thousand prefixes the meta document lists.
//
// cfip.go scans its prefixes linearly, which is right for the thirty-odd
// Cloudflare ranges it holds. The actions list is over seven thousand, and this
// lookup runs once per request on the hot path, so the prefixes are bucketed by
// mask length instead: a lookup masks the address to each length that actually
// occurs and probes a map. That is a handful of map hits rather than thousands
// of Contains calls, and netip.Addr being comparable means the map needs no
// encoding step.
type ipTagSet struct {
	v4, v6 []lenBucket
}

type lenBucket struct {
	bits  int
	addrs map[netip.Addr]string // masked network address -> service tag
}

func newIPTagSet(prefixes []ghPrefix) *ipTagSet {
	byLen := map[bool]map[int]map[netip.Addr]string{true: {}, false: {}}
	for _, p := range prefixes {
		prefix, err := netip.ParsePrefix(p.Prefix)
		if err != nil {
			continue
		}
		prefix = prefix.Masked()
		is4 := prefix.Addr().Is4()
		byBits := byLen[is4]
		if byBits[prefix.Bits()] == nil {
			byBits[prefix.Bits()] = map[netip.Addr]string{}
		}
		byBits[prefix.Bits()][prefix.Addr()] = p.Service
	}

	set := &ipTagSet{}
	for is4, byBits := range byLen {
		buckets := make([]lenBucket, 0, len(byBits))
		for bits, addrs := range byBits {
			buckets = append(buckets, lenBucket{bits: bits, addrs: addrs})
		}
		// Longest mask first, so the most specific range wins when ranges nest.
		slices.SortFunc(buckets, func(a, b lenBucket) int { return b.bits - a.bits })
		if is4 {
			set.v4 = buckets
		} else {
			set.v6 = buckets
		}
	}
	return set
}

// Lookup returns the service tag owning ip, or "" when none does.
func (s *ipTagSet) Lookup(ip netip.Addr) string {
	if s == nil || !ip.IsValid() {
		return ""
	}
	// An IPv4-mapped IPv6 address has to be unwrapped before matching: a v4
	// prefix does not Contain a v6-shaped address, and CF-Connecting-IP arrives
	// in either form.
	ip = ip.Unmap()

	buckets := s.v6
	if ip.Is4() {
		buckets = s.v4
	}
	for _, b := range buckets {
		network, err := ip.Prefix(b.bits)
		if err != nil {
			continue
		}
		if tag, ok := b.addrs[network.Addr()]; ok {
			return tag
		}
	}
	return ""
}

// ghTagSet holds the live set. It is a package-level atomic rather than a field
// threaded through the handlers because AccessLogMiddleware already reaches for
// the Cloudflare prefixes the same way (GetCFIPManager), and one more parameter
// on every handler signature to carry a value that is process-wide anyway buys
// nothing.
var ghTagSet atomic.Pointer[ipTagSet]

// tagDirect marks a request that was checked against the GitHub range list and
// matched nothing. It exists so that an empty tags array means exactly one
// thing - "this row was never classified" - and nothing else.
//
// Without it a non-CI request logged today is indistinguishable from a row
// predating classification, because both carry no tags. The /stats fallback
// would then re-decide it against whatever the range list says later, and a
// range GitHub adds next month would retroactively turn today's ordinary
// traffic into CI traffic. That is the exact failure the tag/dictionary split
// is meant to prevent, so the distinction has to be recorded, not inferred.
const tagDirect = "direct"

// tagUnclassified marks a request that arrived before the range list had ever
// loaded - the process had just started, or every fetch so far had failed.
//
// It is a tag rather than an empty array so the gap is countable: one query
// says exactly how many rows are affected, and scripts/backfill_tags.py can
// repair precisely those. An empty array would be indistinguishable from a row
// predating classification altogether, and after the tags migration there are
// no such rows left - reintroducing the ambiguity would undo that.
const tagUnclassified = "unclassified"

// ghTagsFor returns the access-log tags for a client address.
//
// Three outcomes, and they are three rather than two on purpose: a matched
// address, an address checked and not matched, and an address that could not be
// checked because the list had never loaded. Collapsing the last into either of
// the others would either invent a classification that never happened or hide
// that anything was missed.
func ghTagsFor(ip netip.Addr) []string {
	set := ghTagSet.Load()
	if set == nil {
		return []string{tagUnclassified}
	}
	if tag := set.Lookup(ip); tag != "" {
		return []string{"github_" + tag}
	}
	return []string{tagDirect}
}
