package main

import (
	"fmt"
	"regexp"
	"strings"
)

// pkgRef identifies a Debian source package version.
type pkgRef struct {
	Source    string // source package, e.g. glibc
	Version   string // source version, e.g. 2.41-12+deb13u3
	Component string // main, contrib, non-free, non-free-firmware
}

// binNMU matches the +bN suffix a binary-only rebuild appends. Such a version
// has no .dsc of its own - the sources did not change, only the build did - so
// it has to be stripped before asking for one.
var binNMU = regexp.MustCompile(`\+b[0-9]+$`)

// parseArchive turns the X-DEBUGINFOD-ARCHIVE header into a source package.
//
// debuginfod.debian.net answers /buildid/<id>/debuginfo with the .deb it took
// the file from, as a path on its own mirror:
//
//	/srv/mirror/debian-debug/dbg-main/g/glibc/libc6-dbg_2.41-12+deb13u3_amd64.deb
//
// That header is the whole reason this service can exist without an index of
// its own. Mapping a build ID to a package is otherwise a mirror-sized job -
// it is what cmd/indexer was originally for - and Debian is already doing it.
//
// Two details are easy to get wrong and both come from real headers:
//
//   - The component is dbg-main, not main. Debug packages live in their own
//     component, but the SOURCES live in the plain one, so the dbg- prefix is
//     dropped when building the pool URL.
//   - The source package is the pool DIRECTORY (glibc), never the binary
//     package name in the filename (libc6-dbg). They differ for most packages,
//     and libc6-dbg is also the pre-dbgsym naming, so the filename cannot even
//     be relied on to follow one convention.
func parseArchive(header string) (pkgRef, error) {
	parts := strings.Split(strings.Trim(header, "/"), "/")
	if len(parts) < 4 {
		return pkgRef{}, fmt.Errorf("archive path too short: %q", header)
	}
	// Take the last four components: <component>/<shard>/<source>/<file>.deb.
	// Anchoring on the tail rather than on "/srv/mirror/debian-debug" keeps
	// this working if Debian rearranges its own mirror layout.
	tail := parts[len(parts)-4:]
	component, source, file := tail[0], tail[2], tail[3]

	component = strings.TrimPrefix(component, "dbg-")
	if !strings.HasSuffix(file, ".deb") {
		return pkgRef{}, fmt.Errorf("archive is not a .deb: %q", file)
	}

	// <binary>_<version>_<arch>.deb - the version is what lies between the
	// first and last underscore, because a binary package name may not contain
	// one but an architecture never does.
	name := strings.TrimSuffix(file, ".deb")
	first := strings.Index(name, "_")
	last := strings.LastIndex(name, "_")
	if first < 0 || last <= first {
		return pkgRef{}, fmt.Errorf("cannot read a version out of %q", file)
	}
	version := binNMU.ReplaceAllString(name[first+1:last], "")

	return pkgRef{Source: source, Version: version, Component: component}, nil
}
