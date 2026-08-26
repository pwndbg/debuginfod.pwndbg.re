package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// The proxy listens on loopback and something on the same host forwards traffic
// to it, so RemoteAddr is always localhost. Without trusting loopback,
// CF-Connecting-IP would be ignored and access_log would record the front end's
// address instead of the client's.
func TestIsTrustedAcceptsLoopback(t *testing.T) {
	m := &CFIPManager{} // empty API list - only the hardcoded part matters here
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.53", true}, // the whole /8, not just .1
		{"::1", true},
		{"::ffff:127.0.0.1", true}, // the mapped form from a dual-stack socket
		{"8.8.8.8", false},
		{"172.71.1.1", false}, // a CF prefix, but the list is empty -> not trusted
	} {
		if got := m.IsTrusted(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("IsTrusted(%s)=%v, oczekiwano %v", tc.ip, got, tc.want)
		}
	}
}

// End to end: the header must be honoured from loopback and IGNORED from any
// other address - otherwise anyone could write an arbitrary IP into access_log.
func TestGetRealIPHonoursHeaderOnlyFromTrusted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"zza loopbacku bierzemy naglowek", "127.0.0.1:40000", "203.0.113.7"},
		{"z obcego adresu ignorujemy", "198.51.100.9:40000", "198.51.100.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/buildid/aa/debuginfo", nil)
			r.RemoteAddr = tc.remoteAddr
			r.Header.Set("CF-Connecting-IP", "203.0.113.7")

			got, err := getRealIP(r)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.want {
				t.Errorf("getRealIP=%v, oczekiwano %v", got, tc.want)
			}
		})
	}
}
