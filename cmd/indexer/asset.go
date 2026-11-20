package main

import (
	"strings"

	"github.com/pwndbg/debuginfod.pwndbg.re/pkgarchive"
)

// python-build-standalone names every asset the same way:
//
//	cpython-<python>+<tag>-<triple>-<variant>.tar.<ext>
//	cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-install_only.tar.gz
//	cpython-3.14.7+20260825-aarch64-unknown-linux-gnu-pgo+lto-full.tar.zst
//
// Everything the index needs about an asset is in that name, so nothing has to
// be opened to decide whether it is worth downloading.
type Asset struct {
	Release string // release tag, e.g. "20260825"
	Name    string // asset file name, verbatim
	URL     string // browser_download_url
	Size    int64

	Python  string // "3.14.7"
	Triple  string // "x86_64-unknown-linux-gnu"
	Variant string // "install_only", "install_only_stripped", "pgo+lto-full", ...
}

// osMarkers ends every target triple. Splitting the name on '-' does not work:
// both the triple and the variant contain dashes, and the number of components
// in each varies by platform (x86_64_v3-unknown-linux-gnu vs armv7-unknown-
// linux-gnueabihf). Matching the OS/ABI tail instead is exact.
//
// Longest first, so gnueabihf is not cut short by gnueabi, and gnueabi not by
// gnu.
var osMarkers = []string{
	"unknown-linux-gnueabihf",
	"unknown-linux-gnueabi",
	"unknown-linux-musl",
	"unknown-linux-gnu",
	"apple-darwin",
	"pc-windows-msvc",
	"pc-windows-gnu",
}

// ParseAssetName splits an asset file name. It returns false for anything that
// is not a cpython tarball - a release also carries .sha256 files, SBOMs and
// pypy builds, none of which hold ELF objects we can serve.
func ParseAssetName(name string) (python, triple, variant string, ok bool) {
	rest, found := strings.CutPrefix(name, "cpython-")
	if !found {
		return "", "", "", false
	}
	switch {
	case strings.HasSuffix(rest, ".tar.gz"):
		rest = strings.TrimSuffix(rest, ".tar.gz")
	case strings.HasSuffix(rest, ".tar.zst"):
		rest = strings.TrimSuffix(rest, ".tar.zst")
	default:
		// .sha256, .sbom.json and friends.
		return "", "", "", false
	}

	python, rest, found = strings.Cut(rest, "+")
	if !found {
		return "", "", "", false
	}
	// Drop the release tag; the caller already knows it from the release.
	_, rest, found = strings.Cut(rest, "-")
	if !found {
		return "", "", "", false
	}

	for _, marker := range osMarkers {
		i := strings.Index(rest, marker)
		if i < 0 {
			continue
		}
		end := i + len(marker)
		triple = rest[:end]
		variant = strings.TrimPrefix(rest[end:], "-")
		if variant == "" {
			return "", "", "", false
		}
		return python, triple, variant, true
	}
	return "", "", "", false
}

// IsELFTarget reports whether the triple describes a platform whose binaries
// carry GNU build-ids. macOS uses Mach-O UUIDs and Windows uses PDB GUIDs;
// neither is a build-id, and neither is anything the debuginfod protocol
// addresses, so those assets are skipped rather than downloaded and found empty.
func IsELFTarget(triple string) bool {
	return strings.Contains(triple, "linux")
}

// Archive turns a release asset into the source-neutral form the index stores.
//
// install_only binaries are unstripped, so they answer /executable as well as
// /debuginfo. The header is the asset name: there is no source package to
// find, and a name a person can paste into a browser is the most useful thing
// x-debuginfod-archive can say here.
func (a Asset) Archive() Archive {
	format, _ := pkgarchive.FormatOf(a.Name)
	return Archive{
		Source: sourcePBS, URL: a.URL, Name: a.Name, Size: a.Size, Format: format,
		Package: "cpython", Version: a.Python + "+" + a.Release, Arch: a.Triple,
		Header: a.Name, Kind: kindExecutable,
	}
}
