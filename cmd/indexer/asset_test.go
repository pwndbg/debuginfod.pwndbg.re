package main

import "testing"

// The names here are real. Both the triple and the variant contain dashes and
// neither has a fixed number of components, which is why the parser matches the
// OS/ABI tail instead of splitting on '-'.
func TestParseAssetName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		python  string
		triple  string
		variant string
		ok      bool
	}{
		{
			name:   "cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-install_only.tar.gz",
			python: "3.14.7", triple: "x86_64-unknown-linux-gnu", variant: "install_only", ok: true,
		},
		{
			name:   "cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-install_only_stripped.tar.gz",
			python: "3.14.7", triple: "x86_64-unknown-linux-gnu", variant: "install_only_stripped", ok: true,
		},
		{
			// The variant carries a '+' of its own, so cutting the name at the
			// first '+' has to happen before the variant is looked at.
			name:   "cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-pgo+lto-full.tar.zst",
			python: "3.14.7", triple: "x86_64-unknown-linux-gnu", variant: "pgo+lto-full", ok: true,
		},
		{
			// gnueabihf must not be cut short by the shorter gnu marker, or the
			// variant would come out as "eabihf-lto-full".
			name:   "cpython-3.14.7+20260825-armv7-unknown-linux-gnueabihf-lto-full.tar.zst",
			python: "3.14.7", triple: "armv7-unknown-linux-gnueabihf", variant: "lto-full", ok: true,
		},
		{
			// A microarchitecture level adds a component on the CPU side.
			name:   "cpython-3.12.11+20260825-x86_64_v3-unknown-linux-gnu-install_only.tar.gz",
			python: "3.12.11", triple: "x86_64_v3-unknown-linux-gnu", variant: "install_only", ok: true,
		},
		{
			name:   "cpython-3.14.7+20260825-aarch64-apple-darwin-install_only.tar.gz",
			python: "3.14.7", triple: "aarch64-apple-darwin", variant: "install_only", ok: true,
		},
		{
			name:   "cpython-3.14.7+20260825-x86_64-pc-windows-msvc-install_only.tar.gz",
			python: "3.14.7", triple: "x86_64-pc-windows-msvc", variant: "install_only", ok: true,
		},

		// Everything a release carries besides cpython tarballs.
		{name: "cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-install_only.tar.gz.sha256"},
		{name: "pypy3.11-7.3.20-linux64.tar.bz2"},
		{name: "SHA256SUMS"},
		{name: "cpython-3.14.7-x86_64-unknown-linux-gnu.tar.gz"}, // no +tag
	} {
		t.Run(tc.name, func(t *testing.T) {
			python, triple, variant, ok := ParseAssetName(tc.name)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %q/%q/%q)", ok, tc.ok, python, triple, variant)
			}
			if !ok {
				return
			}
			if python != tc.python || triple != tc.triple || variant != tc.variant {
				t.Errorf("got %q/%q/%q, want %q/%q/%q",
					python, triple, variant, tc.python, tc.triple, tc.variant)
			}
		})
	}
}

// Only ELF platforms carry a GNU build-id, and downloading a macOS or Windows
// asset to discover it holds none is 126 MB of wasted transfer per asset.
func TestIsELFTarget(t *testing.T) {
	elf := []string{
		"x86_64-unknown-linux-gnu", "aarch64-unknown-linux-musl",
		"armv7-unknown-linux-gnueabihf", "x86_64_v3-unknown-linux-gnu",
	}
	notELF := []string{"aarch64-apple-darwin", "x86_64-pc-windows-msvc"}
	for _, tr := range elf {
		if !IsELFTarget(tr) {
			t.Errorf("IsELFTarget(%q) = false, want true", tr)
		}
	}
	for _, tr := range notELF {
		if IsELFTarget(tr) {
			t.Errorf("IsELFTarget(%q) = true, want false", tr)
		}
	}
}
