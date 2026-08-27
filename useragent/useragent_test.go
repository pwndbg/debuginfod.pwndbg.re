package useragent

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func seen(t *testing.T, c *http.Client) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
	}))
	defer srv.Close()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return got
}

// The whole point: a request that sets nothing still identifies us. This is the
// case that kept regressing when the header was set at call sites.
func TestClientSetsTheHeaderWithoutTheCallerDoingAnything(t *testing.T) {
	got := seen(t, Client(nil, "deb"))
	if want := String("deb"); got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
	if got == "" || got[:4] != "pwnd" {
		t.Errorf("outbound request does not identify the project: %q", got)
	}
}

// Guarding against forgetting, not against deciding.
func TestExplicitHeaderIsKept(t *testing.T) {
	c := Client(nil, "deb")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "custom/1.0" {
			t.Errorf("User-Agent = %q, want the caller's custom/1.0", got)
		}
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("User-Agent", "custom/1.0")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// Wrapping must not reach into the shared default client: that would change the
// User-Agent of every unrelated request in the process.
func TestClientDoesNotMutateTheOneItIsGiven(t *testing.T) {
	base := &http.Client{}
	wrapped := Client(base, "nix")
	if base.Transport != nil {
		t.Error("the caller's client was modified in place")
	}
	if wrapped.Transport == nil {
		t.Error("the returned client has no transport")
	}
}

// A component tells an upstream operator which of our services is calling.
func TestStringNamesTheComponent(t *testing.T) {
	if got := String("nix"); got != "pwndbg-debuginfod/nix (+https://github.com/pwndbg/debuginfod.pwndbg.re)" {
		t.Errorf("String(nix) = %q", got)
	}
	if got := String(""); got != "pwndbg-debuginfod (+https://github.com/pwndbg/debuginfod.pwndbg.re)" {
		t.Errorf("String() = %q", got)
	}
}
