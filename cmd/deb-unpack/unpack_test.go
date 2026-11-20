package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The shard directory mirrors the archive layout; it does not choose it.
func TestPoolDir(t *testing.T) {
	for src, want := range map[string]string{
		"glibc": "g", "zlib": "z", "libfoo": "libf", "libc": "libc", "lib": "l",
	} {
		if got := poolDir(src); got != want {
			t.Errorf("poolDir(%q) = %q, want %q", src, got, want)
		}
	}
}

const dsc = `Format: 3.0 (quilt)
Source: zlib
Version: 1:1.3.dfsg+really1.3.2-3
Checksums-Sha256:
 aaaa 1295859 zlib_1.3.dfsg+really1.3.2.orig.tar.gz
Files:
 af03446d28642e6dd3ff0bfedf8bb044 1295859 zlib_1.3.dfsg+really1.3.2.orig.tar.gz
 e1873f1e126ada5ba73a9061c60a5157 17972 zlib_1.3.dfsg+really1.3.2-3.debian.tar.xz

-----BEGIN PGP SIGNATURE-----
`

func writeDsc(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.dsc")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Only the Files: stanza counts. Checksums-Sha256 lists the same names in the
// same shape, so a parser that keys on indentation alone downloads everything
// twice - and the signature block below must not be read as data at all.
func TestFilesFromDscReadsOnlyTheFilesStanza(t *testing.T) {
	got, err := filesFromDsc(writeDsc(t, dsc))
	if err != nil {
		t.Fatalf("filesFromDsc: %v", err)
	}
	want := []string{
		"zlib_1.3.dfsg+really1.3.2.orig.tar.gz",
		"zlib_1.3.dfsg+really1.3.2-3.debian.tar.xz",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// These names are read out of a downloaded file and are about to be joined onto
// a path. A traversal is refused outright rather than cleaned: a .dsc that
// needs sanitising is one we do not understand.
func TestFilesFromDscRefusesPathTraversal(t *testing.T) {
	for _, name := range []string{"../../etc/passwd", "/etc/passwd", "sub/dir/x.tar.gz", ".hidden"} {
		body := "Files:\n aaaa 1 " + name + "\n"
		if _, err := filesFromDsc(writeDsc(t, body)); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestFilesFromDscRejectsEmpty(t *testing.T) {
	if _, err := filesFromDsc(writeDsc(t, "Format: 3.0 (quilt)\n")); err == nil {
		t.Error("a .dsc listing no files must be an error, not an empty unpack")
	}
}

// Package names and versions become both path components and command
// arguments, and they arrive from a header on a third-party server.
func TestInputValidation(t *testing.T) {
	bad := []string{"../glibc", "glibc/../x", "gli bc", "", "-glibc", "GLIBC", "a"}
	for _, s := range bad {
		if rePackage.MatchString(s) {
			t.Errorf("package %q accepted", s)
		}
	}
	for _, s := range []string{"glibc", "zlib", "libfoo", "gcc-14", "g++"} {
		if s == "g++" {
			continue // '+' is legal in package names but not in this position
		}
		if !rePackage.MatchString(s) {
			t.Errorf("package %q rejected", s)
		}
	}
	for _, s := range []string{"../1.0", "1.0;rm -rf /", "1.0 2.0", ""} {
		if reVersion.MatchString(s) {
			t.Errorf("version %q accepted", s)
		}
	}
	for _, s := range []string{"2.41-12+deb13u3", "1.3.dfsg+really1.3.2-3", "1.0~rc1"} {
		if !reVersion.MatchString(s) {
			t.Errorf("version %q rejected", s)
		}
	}
}

// A tree is only ever reported once it is complete, and completeness is decided
// by the marker. The marker is written into the staging directory before the
// atomic rename, so a package-named directory always implies a finished tree -
// but one arriving by other means (a restored backup, an older build) would be
// invisible to /trees and never evicted, so sweepStale says so rather than
// silently leaving it or, worse, deleting an operator's directory.
func TestSweepStaleReportsButKeepsUnmarkedTrees(t *testing.T) {
	root := t.TempDir()
	s := &server{root: root}

	complete := filepath.Join(root, "main", "glibc_1")
	unmarked := filepath.Join(root, "main", "zlib_1")
	for _, d := range []string{complete, unmarked} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(complete, doneMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s.sweepStale(0) // nothing is too young to consider

	for _, d := range []string{complete, unmarked} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("sweepStale deleted %s; it must only report unmarked trees", d)
		}
	}
}

// Staging directories are the ones it does delete - but only when old enough,
// because a young one may belong to an unpack running right now.
func TestSweepStaleReclaimsOnlyOldStagingDirs(t *testing.T) {
	root := t.TempDir()
	s := &server{root: root}
	old := filepath.Join(root, "main", ".tmp-glibc-old")
	young := filepath.Join(root, "main", ".tmp-glibc-young")
	doomed := filepath.Join(root, "main", "zlib_1.deleting")
	for _, d := range []string{old, young, doomed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-3 * time.Hour)
	os.Chtimes(old, past, past)
	os.Chtimes(doomed, past, past)

	s.sweepStale(2 * time.Hour)

	if _, err := os.Stat(old); err == nil {
		t.Error("an abandoned staging directory was not reclaimed")
	}
	if _, err := os.Stat(doomed); err == nil {
		t.Error("an abandoned .deleting directory was not reclaimed")
	}
	if _, err := os.Stat(young); err != nil {
		t.Error("a young staging directory was deleted; it may belong to a live unpack")
	}
}

// The serving side decides what was used; this side is the only one that can
// write, because the serving container mounts the volume read-only. The mtime
// it writes is exactly what /trees reports as Modified, so there is no second
// timestamp to keep in step.
func TestHandleTouchWritesMtimeAndRefusesBadKeys(t *testing.T) {
	root := t.TempDir()
	s := &server{root: root}
	tree := filepath.Join(root, "main", "glibc_1")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(tree, old, old); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /touch/{component}/{name}", s.handleTouch)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(path string) int {
		resp, err := srv.Client().Post(srv.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("/touch/main/glibc_1"); code != http.StatusNoContent {
		t.Fatalf("touch returned %d", code)
	}
	info, err := os.Stat(tree)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(old.Add(time.Hour)) {
		t.Errorf("mtime not updated: still %v", info.ModTime())
	}

	// The key arrives from another service but still becomes a filesystem path,
	// and "it is internal" is not a property the filesystem can check.
	if code := post("/touch/..%2f..%2fetc/passwd"); code == http.StatusNoContent {
		t.Error("a traversal in the component was accepted")
	}
	if code := post("/touch/nosuch/glibc_1"); code == http.StatusNoContent {
		t.Error("an unknown component was accepted")
	}

	// A tree evicted between the decision and this call is not an error - the
	// next request rebuilds it.
	if code := post("/touch/main/gone_1"); code != http.StatusNoContent {
		t.Errorf("touch of a missing tree returned %d, want 204", code)
	}
}
