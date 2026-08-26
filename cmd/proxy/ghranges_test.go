package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func mustPrefixes(t *testing.T, service string, cidrs ...string) []ghPrefix {
	t.Helper()
	out := make([]ghPrefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, ghPrefix{Prefix: c, Service: service})
	}
	return out
}

// The address arrives as either an IPv4 address or an IPv4-mapped IPv6 one,
// depending on whether it came from RemoteAddr or from CF-Connecting-IP, and a
// v4 prefix does not Contain a v6-shaped address. Both spellings have to match
// or CI traffic through Cloudflare - which is all of it - stays untagged.
func TestIPTagSetMatchesBothSpellingsOfIPv4(t *testing.T) {
	set := newIPTagSet(mustPrefixes(t, "actions", "4.148.0.0/16", "2603:1030::/32"))

	for _, addr := range []string{"4.148.1.2", "::ffff:4.148.1.2"} {
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := set.Lookup(ip); got != "actions" {
			t.Errorf("Lookup(%s) = %q, want %q", addr, got, "actions")
		}
	}
	if got := set.Lookup(netip.MustParseAddr("2603:1030::1")); got != "actions" {
		t.Errorf("IPv6 lookup = %q, want actions", got)
	}
	for _, addr := range []string{"8.8.8.8", "2001:db8::1", "4.149.0.1"} {
		if got := set.Lookup(netip.MustParseAddr(addr)); got != "" {
			t.Errorf("Lookup(%s) = %q, want no match", addr, got)
		}
	}
}

// GitHub lists nested ranges. The bucket order has to answer with the most
// specific one, not whichever mask length happens to be probed first.
func TestIPTagSetPrefersTheMostSpecificPrefix(t *testing.T) {
	set := newIPTagSet(append(
		mustPrefixes(t, "actions", "4.148.0.0/16"),
		mustPrefixes(t, "copilot", "4.148.1.0/24")...,
	))
	if got := set.Lookup(netip.MustParseAddr("4.148.1.5")); got != "copilot" {
		t.Errorf("nested lookup = %q, want copilot", got)
	}
	if got := set.Lookup(netip.MustParseAddr("4.148.9.5")); got != "actions" {
		t.Errorf("outer lookup = %q, want actions", got)
	}
}

// A nil set is the state before the first refresh, and every request goes
// through this path.
func TestIPTagSetNilIsSafe(t *testing.T) {
	var set *ipTagSet
	if got := set.Lookup(netip.MustParseAddr("4.148.1.2")); got != "" {
		t.Errorf("nil lookup = %q, want empty", got)
	}
	if got := (&ipTagSet{}).Lookup(netip.Addr{}); got != "" {
		t.Errorf("invalid address lookup = %q, want empty", got)
	}
}

const metaDoc = `{
  "ssh_keys": ["ssh-ed25519 AAAA"],
  "actions": ["4.148.0.0/16", "2603:1030::/32"],
  "copilot": ["20.1.2.0/24"],
  "web": ["140.82.112.0/20"]
}`

func TestParseGitHubMetaSelectsRequestedServices(t *testing.T) {
	got, err := parseGitHubMeta(strings.NewReader(metaDoc), []string{"actions"})
	if err != nil {
		t.Fatalf("parseGitHubMeta: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d prefixes, want 2: %+v", len(got), got)
	}
	for _, p := range got {
		if p.Service != "actions" {
			t.Errorf("prefix %s tagged %q, want actions", p.Prefix, p.Service)
		}
	}
}

func TestParseGitHubMetaAcceptsSeveralServices(t *testing.T) {
	got, err := parseGitHubMeta(strings.NewReader(metaDoc), []string{"actions", "copilot"})
	if err != nil {
		t.Fatalf("parseGitHubMeta: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d prefixes, want 3", len(got))
	}
}

// A misspelled service name has to be an error. Silently returning nothing
// would look exactly like a working refresh that happened to filter nobody.
func TestParseGitHubMetaRejectsUnknownService(t *testing.T) {
	_, err := parseGitHubMeta(strings.NewReader(metaDoc), []string{"action"})
	if err == nil {
		t.Fatal("expected an error for an unknown service key")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("error does not name the key: %v", err)
	}
}

// One malformed CIDR must not cost the rest. These strings become the
// dictionary's key and a single bad one fails the whole load, so they are
// filtered here rather than passed through.
func TestParseGitHubMetaDropsMalformedCIDRs(t *testing.T) {
	doc := `{"actions": ["4.148.0.0/16", "not-a-cidr", "4.149.0.0/999", "20.1.2.0/24"]}`
	got, err := parseGitHubMeta(strings.NewReader(doc), []string{"actions"})
	if err != nil {
		t.Fatalf("parseGitHubMeta: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d prefixes, want 2: %+v", len(got), got)
	}
}

// A document whose requested key is present but empty yields nothing usable,
// and publishing nothing would read as "no CI traffic exists".
func TestParseGitHubMetaRejectsEmptyResult(t *testing.T) {
	if _, err := parseGitHubMeta(strings.NewReader(`{"actions": []}`), []string{"actions"}); err == nil {
		t.Fatal("expected an error for an empty range list")
	}
	if _, err := parseGitHubMeta(strings.NewReader(`{"actions": ["nope"]}`), []string{"actions"}); err == nil {
		t.Fatal("expected an error when every CIDR was unparseable")
	}
}

// CF-IPCountry is client-supplied unless the peer is Cloudflare. Anything that
// can reach the origin port can set it, so believing it from a routable address
// would let a caller write its own country into access_log.
//
// Loopback stands in for Cloudflare here, which is not a shortcut: production
// terminates the tunnel with cloudflared on the same host, so every real
// request arrives from 127.0.0.1 and it is IsTrusted's loopback carve-out that
// makes the header believable at all.
func TestGetCFCountryTrustsHeaderOnlyFromCloudflare(t *testing.T) {
	req := func(remote, country string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/buildid/x/debuginfo", nil)
		r.RemoteAddr = remote
		if country != "" {
			r.Header.Set("CF-IPCountry", country)
		}
		return r
	}

	if got := getCFCountry(req("127.0.0.1:8031", "PL")); got != "PL" {
		t.Errorf("through the tunnel: got %q, want PL", got)
	}
	if got := getCFCountry(req("8.8.8.8:1234", "PL")); got != "" {
		t.Errorf("from a routable peer: got %q, want empty", got)
	}
	if got := getCFCountry(req("127.0.0.1:8031", "")); got != "" {
		t.Errorf("no header: got %q, want empty", got)
	}
	// XX means "could not place" and T1 means Tor; both are real answers and
	// must survive, or the column silently means "known country or nothing".
	for _, c := range []string{"XX", "T1"} {
		if got := getCFCountry(req("127.0.0.1:8031", c)); got != c {
			t.Errorf("%s: got %q, want %s", c, got, c)
		}
	}
}

// Three outcomes, never two. The row's tags are now the only record of how it
// was classified - there is no dictionary left to reconstruct it later - so
// "could not check" has to be as visible as the other two. Folding it into
// tagDirect would assert a classification that never happened; leaving it as an
// empty array would make it uncountable, and after the tags migration nothing
// else in the table has empty tags.
func TestGhTagsForDistinguishesUncheckedFromNotMatched(t *testing.T) {
	ghTagSet.Store(nil)
	if got := ghTagsFor(netip.MustParseAddr("4.148.1.2")); len(got) != 1 || got[0] != tagUnclassified {
		t.Errorf("before any refresh: tags = %v, want [%s]", got, tagUnclassified)
	}

	ghTagSet.Store(newIPTagSet(mustPrefixes(t, "actions", "4.148.0.0/16")))
	t.Cleanup(func() { ghTagSet.Store(nil) })

	if got := ghTagsFor(netip.MustParseAddr("4.148.1.2")); len(got) != 1 || got[0] != "github_actions" {
		t.Errorf("a CI address: tags = %v, want [github_actions]", got)
	}
	if got := ghTagsFor(netip.MustParseAddr("93.184.216.34")); len(got) != 1 || got[0] != tagDirect {
		t.Errorf("an ordinary address: tags = %v, want [%s]", got, tagDirect)
	}
}
