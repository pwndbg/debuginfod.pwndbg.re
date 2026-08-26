package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

type CFIPResponse struct {
	Result struct {
		IPv4CIDRs []string `json:"ipv4_cidrs"`
		IPv6CIDRs []string `json:"ipv6_cidrs"`
	} `json:"result"`
	Success bool `json:"success"`
}

type CFIPManager struct {
	prefixes    []netip.Prefix
	lastUpdated time.Time
}

func NewCFIPManager() *CFIPManager {
	m := &CFIPManager{}
	if err := m.refresh(); err != nil {
		log.WithError(err).Warn("Failed to refresh cloud flare IPs")
	}
	return m
}

func (m *CFIPManager) refresh() error {
	if !m.lastUpdated.IsZero() && m.lastUpdated.Add(time.Hour*24).After(time.Now()) {
		return nil
	}

	const apiURL = "https://api.cloudflare.com/client/v4/ips"

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return fmt.Errorf("http get failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var apiData CFIPResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiData); err != nil {
		return fmt.Errorf("json decode failed: %w", err)
	}

	if !apiData.Success {
		return fmt.Errorf("cloudflare api returned success=false")
	}

	var newPrefixes []netip.Prefix
	for _, cidrs := range [][]string{apiData.Result.IPv4CIDRs, apiData.Result.IPv6CIDRs} {
		for _, cidr := range cidrs {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				log.WithField("cidr", cidr).Error("failed to parse prefix")
				continue
			}
			newPrefixes = append(newPrefixes, prefix)
		}
	}

	m.prefixes = newPrefixes
	m.lastUpdated = time.Now()

	log.Infof("Refreshed CFIPManager, %d prefixes refreshed", len(newPrefixes))
	return nil
}

func (m *CFIPManager) Worker(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := m.refresh(); err != nil {
				log.WithError(err).Warn("Failed to refresh cloud flare IPs")
			}
		case <-ctx.Done():
			return
		}
	}
}

func (m *CFIPManager) IsTrusted(ip netip.Addr) bool {
	// Loopback is trusted unconditionally. The proxy listens on 127.0.0.1:8031
	// (the LISTEN_IP/PORT defaults, which run.sh does not override), so it is
	// not reachable from the internet - TLS terminates somewhere on the same
	// host and forwards to localhost. For that traffic RemoteAddr is always
	// loopback, and the Cloudflare API list will never cover it: without this
	// carve-out CF-Connecting-IP would be ignored and access_log would record
	// the front end's address instead of the client's.
	//
	// IsLoopback rather than explicit 127.0.0.0/8 and ::1/128 prefixes: that is
	// the same set (IPv4 has the whole /8 as loopback, IPv6 exactly one address)
	// but it also covers the ::ffff:127.0.0.1 form, which the IPv4 prefix does
	// NOT match - netip.Prefix.Contains returns false across address families.
	// It therefore keeps working if LISTEN_IP ever points at a dual-stack socket.
	//
	// The price: any process on this host can put an arbitrary IP in the header
	// and it lands in access_log. The container runs with --network host, so
	// "local" here means the whole machine.
	if ip.IsLoopback() {
		return true
	}
	if prefixes := m.prefixes; len(prefixes) > 0 {
		for _, prefix := range prefixes {
			if prefix.Contains(ip) {
				return true
			}
		}
	}
	return false
}

var (
	cfipMgrInstance     *CFIPManager
	cfipMgrInstanceOnce sync.Once
)

func GetCFIPManager() *CFIPManager {
	cfipMgrInstanceOnce.Do(func() {
		cfipMgrInstance = NewCFIPManager()
		go cfipMgrInstance.Worker(context.Background())
	})
	return cfipMgrInstance
}

func getRealIP(r *http.Request) (netip.Addr, error) {
	ipMgr := GetCFIPManager()
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	remoteIP := ap.Addr()

	cfIPHeader := r.Header.Get("CF-Connecting-IP")
	if cfIPHeader != "" && ipMgr.IsTrusted(remoteIP) {
		if clientIP, err := netip.ParseAddr(cfIPHeader); err == nil {
			return clientIP, nil
		}
	}

	return remoteIP, nil
}

// getCFCountry returns the country Cloudflare assigned to the client, or "".
//
// It repeats getRealIP's trust check rather than assuming that a request which
// yielded a CF-Connecting-IP also has a believable country: both headers are
// trivially forgeable by anything that can reach the origin port, and the only
// thing that makes either of them evidence is that the peer is Cloudflare.
// Deliberately not folded into getRealIP - that function returns an address and
// callers that only want the address should not have to ignore a second value.
//
// Empty is also the normal answer when Cloudflare's IP geolocation setting is
// off, so an empty country means "not known", never "not from Cloudflare".
func getCFCountry(r *http.Request) string {
	country := r.Header.Get("CF-IPCountry")
	if country == "" {
		return ""
	}
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !GetCFIPManager().IsTrusted(ap.Addr()) {
		return ""
	}
	// XX is what Cloudflare sends for a client it could not place, and T1 for
	// Tor. Both are kept: they say something, and dropping them would make the
	// column silently mean "known country or nothing".
	return country
}
