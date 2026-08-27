package main

import "testing"

// Every header here is real, taken from debuginfod.debian.net.
func TestParseArchive(t *testing.T) {
	for _, tc := range []struct {
		name, header          string
		source, version, comp string
		wantErr               bool
	}{
		{
			// The case that shaped the parser. Note dbg-main, not main: debug
			// packages have their own component but the SOURCES live in the
			// plain one, so the prefix has to go before a pool URL is built.
			name:   "glibc, legacy -dbg naming",
			header: "/srv/mirror/debian-debug/dbg-main/g/glibc/libc6-dbg_2.41-12+deb13u3_amd64.deb",
			source: "glibc", version: "2.41-12+deb13u3", comp: "main",
		},
		{
			name:   "zlib, modern -dbgsym naming",
			header: "/srv/mirror/debian-debug/main/z/zlib/zlib1g-dbgsym_1.3.dfsg+really1.3.2-3_amd64.deb",
			source: "zlib", version: "1.3.dfsg+really1.3.2-3", comp: "main",
		},
		{
			// The source package is the pool DIRECTORY, never the binary name.
			// libc6-dbg and zlib1g-dbgsym are both binary packages that share
			// nothing with the source name, and taking the filename would send
			// every lookup to a package that does not exist.
			name:   "lib package keeps its own shard",
			header: "/srv/mirror/debian-debug/dbg-main/libf/libfoo/libfoo1-dbgsym_1.2-3_arm64.deb",
			source: "libfoo", version: "1.2-3", comp: "main",
		},
		{
			// A binary-only rebuild has no .dsc of its own - the sources did
			// not change - so asking for one would 404 on every binNMU'd
			// package in the archive.
			name:   "binNMU suffix is stripped",
			header: "/srv/mirror/debian-debug/dbg-main/g/glibc/libc6-dbg_2.41-12+b1_amd64.deb",
			source: "glibc", version: "2.41-12", comp: "main",
		},
		{
			name:   "contrib keeps its component",
			header: "/srv/mirror/debian-debug/dbg-contrib/n/nvidia/foo-dbgsym_1-1_amd64.deb",
			source: "nvidia", version: "1-1", comp: "contrib",
		},

		{name: "too short", header: "/srv/mirror/x.deb", wantErr: true},
		{name: "not a deb", header: "/srv/mirror/debian-debug/main/z/zlib/zlib_1.3.dsc", wantErr: true},
		{name: "no version", header: "/srv/mirror/debian-debug/main/z/zlib/zlib1g.deb", wantErr: true},
		{name: "empty", header: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArchive(tc.header)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArchive: %v", err)
			}
			if got.Source != tc.source || got.Version != tc.version || got.Component != tc.comp {
				t.Errorf("got %s/%s (%s), want %s/%s (%s)",
					got.Source, got.Version, got.Component, tc.source, tc.version, tc.comp)
			}
		})
	}
}
