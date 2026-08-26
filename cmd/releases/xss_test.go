package main

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// Everything on this page that is not a number is a string a client chose: the
// asset filename and the version come straight out of the request path, the
// country out of a request header, and classifyAsset derives the "variant" bucket
// from an arbitrary prefix of the filename - so that bucket is attacker-controlled
// too, not a fixed vocabulary.
//
// Substring assertions are not enough here: breaking out of title="..." needs no
// angle bracket at all. This parses the result and asserts on the DOM.
var xssPayloads = []string{
	`<script>alert(1)</script>`,
	`"><script>alert(1)</script>`,
	`"><img src=x onerror=alert(1)>`,
	`</span><img src=x onerror=alert(1)>`,
	`' onmouseover='alert(1)`,
	`" onmouseover="alert(1)`,
	`</figure><iframe src=javascript:alert(1)>`,
	`javascript:alert(1)`,
	"`+alert(1)+`",
	`${alert(1)}`,
	`$%7BFILE%7D`, // seen in real traffic - an installer with an unexpanded variable
	`</title></style></script>`,
	`<script>alert(1)</script>`,
}

var urlAttrs = map[string]bool{
	"href": true, "src": true, "action": true, "formaction": true,
	"xlink:href": true, "data": true, "srcdoc": true, "poster": true,
}

func renderWithPayloads(t *testing.T) *html.Node {
	t.Helper()
	s := makeSnapshot(7)
	for i, p := range xssPayloads {
		n := uint64(len(xssPayloads) - i)
		s.Versions = append(s.Versions, labelCount{p, n})
		s.Assets = append(s.Assets, labelCount{p, n})
		s.Countries = append(s.Countries, labelCount{p, n})
		s.Clients = append(s.Clients, labelCount{p, n})
		// Release tags come from the GitHub API rather than from a request, but
		// they are still a string this process did not choose.
		s.Curves = append(s.Curves, releaseCurve{Tag: p, Series: []float64{0, float64(n)}, Total: n})
		// Variant and version are closed vocabularies now, so a payload cannot
		// reach them through the collector. Injected directly anyway: the renderer
		// must not depend on its callers for escaping.
		s.Variants = append(s.Variants, labelCount{p, n})
		s.Platforms = append(s.Platforms, labelCount{p, n})
		s.Formats = append(s.Formats, labelCount{p, n})
	}
	s.GhLatestTag = `<script>alert('tag')</script>`
	s.CurveDays = 2

	doc, err := html.Parse(strings.NewReader(string(renderStats(s))))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func TestRenderedPageHasNoInjectedNodes(t *testing.T) {
	doc := renderWithPayloads(t)

	var scripts, bad []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script":
				var sb strings.Builder
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.TextNode {
						sb.WriteString(c.Data)
					}
				}
				scripts = append(scripts, sb.String())
			case "iframe", "img", "object", "embed", "form", "base", "link", "meta":
				if n.Data != "meta" {
					bad = append(bad, "<"+n.Data+">")
				}
			}
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				if strings.HasPrefix(key, "on") {
					bad = append(bad, n.Data+"["+a.Key+"]")
				}
				// Only where the value is dereferenced as a URL. A "javascript:"
				// sitting in title= is tooltip text and navigates nothing - the
				// payloads are supposed to show up there verbatim.
				if urlAttrs[key] && strings.Contains(strings.ToLower(a.Val), "javascript:") {
					bad = append(bad, n.Data+"["+a.Key+"=javascript:]")
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(bad) > 0 {
		t.Errorf("injected nodes/attributes in the DOM: %v", bad)
	}
	if len(scripts) != 1 {
		t.Fatalf("%d script elements, want exactly 1 (the page's own)", len(scripts))
	}
	if scripts[0] != statsJS {
		t.Error("the only script element is not the page's own script")
	}
}

// The payloads must still be readable as text - escaping that silently dropped
// them would pass the test above while making the page useless.
func TestPayloadsSurviveAsText(t *testing.T) {
	doc := renderWithPayloads(t)

	var text strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.Data == "script" {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	for _, p := range []string{`<script>alert(1)</script>`, `' onmouseover='alert(1)`, `$%7BFILE%7D`} {
		if !strings.Contains(text.String(), p) {
			t.Errorf("payload %q did not survive as visible text", p)
		}
	}
}

// The CSP pins the inline script by hash. If the script changes and the hash is
// computed from something else, the browser silently drops the script and every
// chart loses its tooltip - so the two must be derived from the same bytes.
func TestCSPHashMatchesTheEmbeddedScript(t *testing.T) {
	page := string(renderStats(makeSnapshot(7)))

	start := strings.LastIndex(page, "<script>")
	end := strings.LastIndex(page, "</script>")
	if start < 0 || end < start {
		t.Fatal("no inline script in the rendered page")
	}
	embedded := page[start+len("<script>") : end]

	sum := sha256.Sum256([]byte(embedded))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if !strings.Contains(statsCSP, want) {
		t.Errorf("CSP does not carry the hash of the embedded script\n CSP: %s\nwant: %s", statsCSP, want)
	}
	for _, d := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(statsCSP, d) {
			t.Errorf("CSP missing %q", d)
		}
	}
}
