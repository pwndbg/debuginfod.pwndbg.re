package main

import "strings"

// Asset names look like this:
//
//	pwndbg_2026.02.18_x86_64-portable.tar.xz
//	pwndbg-lldb_2026.02.18_amd64.deb
//	pwndbg_2026.02.18_macos_arm64-portable.tar.xz
//
// i.e. <variant>_<version>_<platform>.<format>, where <platform> may itself
// contain an underscore. Nothing guarantees that shape - it is a release-workflow
// convention, not a contract - so every field falls back to "other" rather than
// guessing, and the "other" bucket showing up on the page is the signal that the
// naming changed and this file needs a look.
//
// The split lives in Go rather than in SQL so it can be unit-tested against real
// names, and so that reclassifying does not mean rewriting stored rows: the access
// log keeps the filename verbatim and the buckets are derived at query time.
type assetClass struct {
	Variant  string // pwndbg-gdb, pwndbg-lldb, other
	Platform string // linux/x86_64, macos/arm64, ...
	Format   string // .deb, .tar.xz, ...
}

const unknownBucket = "other"

// knownVariants is a closed vocabulary: only the two things we actually ship are
// their own bar, everything else is "other". Taking the variant as "whatever
// precedes the first underscore" would make it an arbitrary client-supplied
// string - it comes out of the request path - which is both a chart nobody can
// read and the one bucket on the page that an attacker could name.
//
// The gdb build is called pwndbg_* in the release assets; it is shown as
// pwndbg-gdb so the contrast with pwndbg-lldb is visible at a glance.
var knownVariants = map[string]string{
	"pwndbg":      "pwndbg-gdb",
	"pwndbg-gdb":  "pwndbg-gdb",
	"pwndbg-lldb": "pwndbg-lldb",
}

// knownFormats is ordered longest-first: ".tar.xz" has to win over a hypothetical
// ".xz", or every tarball would land in the wrong bucket.
var knownFormats = []string{
	".tar.xz", ".tar.gz", ".tar.zst", ".tar.bz2",
	".AppImage", ".sha256", ".deb", ".rpm", ".apk", ".zip", ".sig",
}

func classifyAsset(name string) assetClass {
	c := assetClass{Variant: unknownBucket, Platform: unknownBucket, Format: unknownBucket}
	if name == "" {
		return c
	}

	base := name
	for _, f := range knownFormats {
		if strings.HasSuffix(name, f) {
			c.Format = f
			base = strings.TrimSuffix(name, f)
			break
		}
	}

	// parts[0] is the variant, parts[1] the version, everything after it the
	// platform.
	parts := strings.Split(base, "_")
	if v, ok := knownVariants[parts[0]]; ok {
		c.Variant = v
	}
	if len(parts) < 3 {
		return c
	}

	plat := strings.Join(parts[2:], "_")
	// "-portable" describes packaging, not a platform; keeping it would split
	// every architecture into two buckets that mean the same machine.
	plat = strings.TrimSuffix(plat, "-portable")

	osName := "linux"
	for _, p := range []string{"macos", "darwin"} {
		if rest, ok := strings.CutPrefix(plat, p+"_"); ok {
			osName, plat = "macos", rest
			break
		}
	}

	switch plat {
	case "x86_64", "amd64", "x64":
		c.Platform = osName + "/x86_64"
	case "arm64", "aarch64":
		c.Platform = osName + "/arm64"
	case "":
		// No platform component at all - a source tarball or a checksum file.
		c.Platform = "any"
	default:
		c.Platform = unknownBucket
	}
	return c
}
