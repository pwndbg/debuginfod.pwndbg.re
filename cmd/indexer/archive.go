package main

// Archive is one downloadable file the index points into: a python-build-
// standalone release asset, or a .deb from an Ubuntu pool. Everything a serving
// backend needs to fetch it again and find a member is recorded here, so
// serving never has to know which source an archive came from.
type Archive struct {
	Source string // "pbs" or "ubuntu" - also the path prefix cmd/index-debuginfod serves under
	URL    string // where to fetch it; also the resume key
	Name   string // file name, verbatim
	Size   int64  // compressed size when the listing states it, else 0
	Format string // pkgarchive format: deb, tar.gz, tar.zst, tar.xz

	Package string // "cpython" or the binary package, e.g. libc6-dbg
	Version string // "3.14.7+20260825" or "2.43-2ubuntu2.7"
	Arch    string // target triple or Debian architecture

	// Header is what x-debuginfod-archive will say for every build ID in this
	// archive. For a .deb it is a pool-shaped path,
	// /ubuntu/pool/main/g/glibc/libc6-dbg_2.43-2ubuntu2.7_amd64.deb, because
	// cmd/deb-debuginfod parses exactly that shape to find the source package -
	// which is how sources for these build IDs come for free.
	Header string

	Kind string // kindExecutable or kindDebuginfo

	// Fallback is a second url for the same bytes, recorded beside URL without
	// a second download. The Ubuntu pool drops a version the moment the next
	// update lands, while Launchpad keeps every file of the primary archive -
	// so a pool file indexed today would be a 404 after the next SRU, until a
	// later run noticed. With both rows the server moves on by itself.
	Fallback string
}

// What a member of an archive is, which decides the endpoints it may answer.
const (
	// kindExecutable: an unstripped binary. It answers /debuginfo - the debug
	// sections are in it - and /executable alike. python-build-standalone's
	// install_only assets are this.
	kindExecutable = "executable"
	// kindDebuginfo: a separate debug file, /usr/lib/debug/.build-id/xx/....debug.
	// Its code sections are NOBITS, so it must never be served as /executable.
	kindDebuginfo = "debuginfo"
)

// Sources the indexer knows. cmd/index-debuginfod refuses any other prefix.
const (
	sourcePBS    = "pbs"
	sourceUbuntu = "ubuntu"
)
