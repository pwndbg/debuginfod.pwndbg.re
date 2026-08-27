// Package useragent makes every outbound HTTP request identify this project.
//
// It exists because setting the header at each call site does not work. The
// proxy learned that once already: its resolution probes went out with Go's
// default Go-http-client/2.0 for as long as nobody noticed, because the header
// was set where the artifact was fetched and not where the upstream was probed.
// The nix backend had the same hole - three requests to cache.nixos.org with no
// User-Agent at all.
//
// So the header is set in a RoundTripper rather than by callers. A new request
// cannot omit it, because no request passes through the client without it.
package useragent

import (
	"fmt"
	"net/http"
)

// project is what a third-party operator sees in their logs, with somewhere to
// go if our traffic is a problem for them. That link is the point: an
// unattributed client that misbehaves gets blocked, an attributed one gets an
// email first.
const project = "pwndbg-debuginfod"

const projectURL = "https://github.com/pwndbg/debuginfod.pwndbg.re"

// String builds the header value for one component. The component is named so
// an upstream can tell which of our services is calling - "nix" fetching from
// cache.nixos.org is a different conversation than the proxy probing them.
func String(component string) string {
	if component == "" {
		return fmt.Sprintf("%s (+%s)", project, projectURL)
	}
	return fmt.Sprintf("%s/%s (+%s)", project, component, projectURL)
}

// Transport wraps rt so every request through it carries the header.
//
// A request that already sets User-Agent keeps it: this guards against
// forgetting, not against deciding.
func Transport(rt http.RoundTripper, component string) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &transport{base: rt, ua: String(component)}
}

// Client returns c with its transport wrapped. c may be nil, and the returned
// client is a copy - wrapping http.DefaultClient in place would change the
// User-Agent of every unrelated request in the process.
func Client(c *http.Client, component string) *http.Client {
	out := &http.Client{}
	if c != nil {
		*out = *c
	}
	out.Transport = Transport(out.Transport, component)
	return out
}

type transport struct {
	base http.RoundTripper
	ua   string
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		// Cloned because RoundTrip must not modify the request it is given;
		// the caller may reuse it, and the http package documents this.
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	return t.base.RoundTrip(req)
}
