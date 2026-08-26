package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"slices"
	"testing"
	"time"
)

// The certificate has to cover the address the proxy dials - 127.0.0.1 - or
// every request fails on the name, not on the chain, which is a much more
// confusing error than the one skipping verification is meant to silence.
func TestSelfSignedCertCoversTheListenAddress(t *testing.T) {
	cert, err := selfSignedCert(certHosts("127.0.0.1:8034"))
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	for _, name := range []string{"127.0.0.1", "localhost"} {
		if _, err := cert.Leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err != nil {
			// Verify with DNSName does not accept an IP; check the SAN directly.
			if ip := net.ParseIP(name); ip != nil {
				if !slices.ContainsFunc(cert.Leaf.IPAddresses, func(x net.IP) bool { return x.Equal(ip) }) {
					t.Errorf("no IP SAN for %s", name)
				}
				continue
			}
			t.Errorf("name %s not covered: %v", name, err)
		}
	}
}

// A cert that expires while the process is up would start failing handshakes
// nobody could act on, since the only rotation is a restart.
func TestSelfSignedCertOutlivesAnyPlausibleUptime(t *testing.T) {
	cert, err := selfSignedCert(certHosts("127.0.0.1:8034"))
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(cert.Leaf.NotAfter) < 365*24*time.Hour {
		t.Errorf("expires in %s, too soon for a process that only rotates on restart",
			time.Until(cert.Leaf.NotAfter).Round(time.Hour))
	}
	// Backdated, so a clock skew cannot make it "not yet valid".
	if !cert.Leaf.NotBefore.Before(time.Now()) {
		t.Errorf("NotBefore is %s, not in the past", cert.Leaf.NotBefore)
	}
}

// It must actually work as a server certificate, and it must negotiate HTTP/2 -
// which is the whole reason TLS is here.
func TestSelfSignedCertServesHTTP2(t *testing.T) {
	cert, err := selfSignedCert(certHosts("127.0.0.1:8034"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- "accept: " + err.Error()
			return
		}
		defer c.Close()
		tc := c.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			done <- "handshake: " + err.Error()
			return
		}
		done <- tc.ConnectionState().NegotiatedProtocol
	}()

	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		InsecureSkipVerify: true, // the point of a self-signed certificate
		NextProtos:         []string{"h2", "http/1.1"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()

	if got := <-done; got != "h2" {
		t.Errorf("negotiated %q, want h2 - TLS is here for HTTP/2", got)
	}
}
