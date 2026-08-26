package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"time"
)

// selfSignedCert makes a certificate for this process, in memory, at startup.
//
// Nothing verifies it, and nothing is meant to: this service listens on loopback
// and is reached only by the proxy on the same host, so the certificate is not
// establishing identity - it is the price of speaking TLS at all. TLS is here
// because Go's HTTP/2 server only runs over it (h2c needs its own wiring), and
// HTTP/2 is what carries 103 Early Hints to a client that keeps one connection
// open for many build IDs.
//
// It follows that the caller MUST skip verification. cmd/proxy's upstream entry
// for nix therefore has to become https:// with a client that does not check the
// chain, or every request fails with "certificate signed by unknown authority".
//
// Held only in memory: a new one on every start. There is nothing to rotate,
// nothing to leak to disk, and no file whose permissions could be got wrong.
func selfSignedCert(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "pwndbg-debuginfod nix backend"},
		// Backdated a minute so a clock skew between this process and whatever
		// starts talking to it a moment later cannot make the certificate
		// "not yet valid".
		NotBefore: now.Add(-time.Minute),
		// Long, because the only rotation is a restart. A short lifetime would
		// mean a process that stays up for months starts failing handshakes for
		// no reason anyone could act on.
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("creating certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parsing certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// certHosts is the set of names the certificate has to cover, derived from the
// listen address so that changing LISTEN_ADDR does not silently produce a
// certificate for the wrong host.
func certHosts(listenAddr string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if host, _, err := net.SplitHostPort(listenAddr); err == nil && host != "" {
		found := false
		for _, h := range hosts {
			if h == host {
				found = true
				break
			}
		}
		if !found {
			hosts = append(hosts, host)
		}
	}
	return hosts
}
