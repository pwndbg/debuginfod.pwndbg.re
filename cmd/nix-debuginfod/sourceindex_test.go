package main

import (
	"os"
	"path/filepath"
	"testing"
)

// tree materialises files under a fresh directory and returns it.
func tree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Matching runs from the right, so neither side has to guess how many
// components to strip. These are the shapes that broke left-side matching.
func TestSourceIndexMatchesFromTheRight(t *testing.T) {
	root := tree(t,
		"glibc-2.42/sysdeps/unix/sysv/linux/getsysstats.c",
		"glibc-2.42/misc/getsysstats.c",
		"glibc-2.42/elf/dl-find_object.c",
		"getsysstats.c",
	)
	ix, err := newSourceIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, req, want string }{
		// The tree carries a package directory the request knows nothing about.
		{"tree has an extra prefix", "/build/../sysdeps/unix/sysv/linux/getsysstats.c",
			"glibc-2.42/sysdeps/unix/sysv/linux/getsysstats.c"},
		// The request carries a sandbox prefix the tree knows nothing about.
		{"request has an extra prefix", "/build/glibc-2.42/misc/getsysstats.c",
			"glibc-2.42/misc/getsysstats.c"},
		{"both sides differ", "/nix/build-dir/elf/dl-find_object.c",
			"glibc-2.42/elf/dl-find_object.c"},
		// Nothing but a basename: the shallowest wins, deterministically.
		{"bare basename", "/getsysstats.c", "getsysstats.c"},
		{"unknown file", "/nothing/like/this.c", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := ""
			if tc.want != "" {
				want = filepath.Join(root, tc.want)
			}
			if got := ix.find(tc.req); got != want {
				t.Errorf("find(%q) = %q, want %q", tc.req, got, want)
			}
		})
	}
}

// Both trees are scored together rather than searched in turn. Searched in turn,
// the overlay answers first with whatever it has - here a bare basename hit in
// an unrelated directory - and .source never gets to offer the far better match.
func TestSourceIndexScoresBothTreesTogether(t *testing.T) {
	overlay := tree(t, "misc/getsysstats.c")
	source := tree(t, "glibc-2.42/sysdeps/unix/sysv/linux/getsysstats.c")

	ix, err := newSourceIndex(overlay, source)
	if err != nil {
		t.Fatal(err)
	}

	got := ix.find("/build/../sysdeps/unix/sysv/linux/getsysstats.c")
	want := filepath.Join(source, "glibc-2.42/sysdeps/unix/sysv/linux/getsysstats.c")
	if got != want {
		t.Errorf("find = %q, want the longer match in .source (%q)", got, want)
	}
}

// On an equal match the overlay wins: it holds what the build actually compiled,
// so the pristine copy would be source that does not match the binary.
func TestSourceIndexOverlayWinsATie(t *testing.T) {
	const rel = "glibc-2.42/misc/getsysstats.c"
	overlay := tree(t, rel)
	source := tree(t, rel)

	ix, err := newSourceIndex(overlay, source)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ix.find("/build/"+rel), filepath.Join(overlay, rel); got != want {
		t.Errorf("find = %q, want the overlay copy (%q)", got, want)
	}
}

// A package with no overlay must still serve its pristine sources, so an
// unreadable tree is skipped rather than failing the index.
func TestSourceIndexToleratesAMissingTree(t *testing.T) {
	source := tree(t, "pkg/main.c")
	ix, err := newSourceIndex(filepath.Join(t.TempDir(), "no-such-overlay"), source)
	if err != nil {
		t.Fatalf("a missing overlay must not fail the index: %v", err)
	}
	if got, want := ix.find("/build/pkg/main.c"), filepath.Join(source, "pkg/main.c"); got != want {
		t.Errorf("find = %q, want %q", got, want)
	}
}

// The answer always comes from walking a tree, never from the client's string
// joined onto a root - so a request cannot name a file outside them whatever it
// contains. That is structural, not a filter to be forgotten.
func TestSourceIndexNeverEscapesTheTree(t *testing.T) {
	ix, err := newSourceIndex(tree(t, "real.c"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"/../../../../etc/passwd",
		"/etc/passwd",
		"/root/../../../../etc/shadow",
	} {
		if got := ix.find(bad); got != "" {
			t.Errorf("find(%q) = %q, want no match", bad, got)
		}
	}
}
