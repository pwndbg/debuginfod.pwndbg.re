package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// tarGz builds a gzipped tar in memory. entries maps member path to contents.
func tarGz(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func elfWithID(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	return makeELF(2, binary.LittleEndian, raw, 0)
}

func TestScanTarballFindsBuildIDs(t *testing.T) {
	const (
		idPython = "64c83ec8756194b5e4d9107164d7c28a1f83d750"
		idLibpy  = "00697e4e6391b0c576e54d293948cd7b0557cd82"
	)
	body := tarGz(t, map[string][]byte{
		"python/bin/python3.14":             elfWithID(t, idPython),
		"python/lib/libpython3.14.so.1.0":   elfWithID(t, idLibpy),
		"python/lib/python3.14/os.py":       []byte("import sys\n"),
		"python/lib/python3.14/LICENSE.txt": []byte("PSF\n"),
	})

	arts, err := ScanTarball("cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-install_only.tar.gz",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ScanTarball: %v", err)
	}

	got := map[string]string{}
	for _, a := range arts {
		got[a.BuildID] = a.Member
	}
	want := map[string]string{
		idPython: "python/bin/python3.14",
		idLibpy:  "python/lib/libpython3.14.so.1.0",
	}
	if len(got) != len(want) {
		t.Fatalf("found %d build ids, want %d: %v", len(got), len(want), got)
	}
	for id, member := range want {
		if got[id] != member {
			t.Errorf("%s -> %q, want %q", id, got[id], member)
		}
	}
}

// Most of an install_only tarball is .py files and stdlib data. A member that is
// not an ELF, or an ELF with no note, has to come back as a skip rather than an
// error - otherwise a single one of them fails the whole asset and the release
// indexes as empty.
func TestScanTarballSkipsNonELFWithoutFailing(t *testing.T) {
	const id = "544a5489c8d113e3639ccbd8d417af24dcbb3722"
	noNote := elfWithID(t, id)
	binary.LittleEndian.PutUint16(noNote[56:], 0) // e_phnum = 0

	body := tarGz(t, map[string][]byte{
		"python/README":             []byte(strings.Repeat("text\n", 100)),
		"python/empty":              {},
		"python/build/trampoline.o": noNote,
		"python/bin/python3.14":     elfWithID(t, id),
	})

	arts, err := ScanTarball("x.tar.gz", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ScanTarball: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("found %d artifacts, want 1: %+v", len(arts), arts)
	}
	if arts[0].Member != "python/bin/python3.14" {
		t.Errorf("member = %q, want python/bin/python3.14", arts[0].Member)
	}
}

func TestScanTarballRejectsUnknownCompression(t *testing.T) {
	if _, err := ScanTarball("x.tar.bz2", bytes.NewReader(nil)); err == nil {
		t.Fatal("expected an error for an unsupported archive")
	}
}

// A real asset, when one is available locally. This is the only test that sees
// a genuine python-build-standalone tarball - the synthetic ones above cannot
// catch a change in how the release pipeline lays them out.
//
//	PBS_TEST_TARBALL=/path/to/cpython-...-install_only.tar.gz go test ./cmd/indexer -run Real -v
func TestScanTarballAgainstRealAsset(t *testing.T) {
	path := os.Getenv("PBS_TEST_TARBALL")
	if path == "" {
		t.Skip("set PBS_TEST_TARBALL to a python-build-standalone tarball")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	arts, err := ScanTarball(path, f)
	if err != nil {
		t.Fatalf("ScanTarball: %v", err)
	}
	if len(arts) == 0 {
		t.Fatal("no build ids found in a real asset")
	}
	var sawInterpreter bool
	for _, a := range arts {
		if !isHexBuildID(a.BuildID) {
			t.Errorf("member %s: implausible build id %q", a.Member, a.BuildID)
		}
		if strings.Contains(a.Member, "/bin/python3") {
			sawInterpreter = true
		}
	}
	if !sawInterpreter {
		t.Error("no interpreter binary among the members")
	}
	t.Logf("%s: %d build ids", path, len(arts))
}

// isHexBuildID guards the table key: these strings are what a client will look
// up by, so a malformed one is a row nothing can ever match.
func isHexBuildID(s string) bool {
	if len(s) < 32 || len(s) > 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// The premise of this whole command: uv ships install_only_stripped, we serve
// out of install_only, and the two agree on every build id. Stripping removes
// DWARF but not .note.gnu.build-id, so the ids and the member paths have to
// match exactly - if they ever stopped matching, every lookup would miss and
// the index would be silently useless rather than visibly broken.
//
//	PBS_TEST_TARBALL=...install_only.tar.gz \
//	PBS_TEST_TARBALL_STRIPPED=...install_only_stripped.tar.gz \
//	  go test ./cmd/indexer -run ShareBuildIDs -v
func TestStrippedAndFullShareBuildIDs(t *testing.T) {
	full, stripped := os.Getenv("PBS_TEST_TARBALL"), os.Getenv("PBS_TEST_TARBALL_STRIPPED")
	if full == "" || stripped == "" {
		t.Skip("set PBS_TEST_TARBALL and PBS_TEST_TARBALL_STRIPPED to matching assets")
	}

	scan := func(path string) map[string]string {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		arts, err := ScanTarball(path, f)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out := map[string]string{}
		for _, a := range arts {
			out[a.BuildID] = a.Member
		}
		return out
	}

	a, b := scan(full), scan(stripped)
	if len(a) == 0 {
		t.Fatal("no build ids in the unstripped asset")
	}
	for id, member := range b {
		got, ok := a[id]
		if !ok {
			t.Errorf("build id %s (%s) is only in the stripped asset", id, member)
			continue
		}
		// The member path has to match too: the index records the path from
		// whichever asset was scanned, and the server opens the other one.
		if got != member {
			t.Errorf("build id %s: %q unstripped vs %q stripped", id, got, member)
		}
	}
	if len(a) != len(b) {
		t.Errorf("%d build ids unstripped, %d stripped", len(a), len(b))
	}
	t.Logf("%d build ids agree across both assets", len(b))
}

// Launchpad's librarian labels .gz files Content-Encoding: gzip whatever the
// request asked for. Left to itself Go's transport decodes that away, and the
// scan then reads a plain tar as the tar.gz Format promised - a failure on
// every such archive, recorded in the resume log as a broken download.
func TestScanArchiveReadsTheBytesAsStored(t *testing.T) {
	const id = "64c83ec8756194b5e4d9107164d7c28a1f83d750"
	body := tarGz(t, map[string][]byte{"python/bin/python3.14": elfWithID(t, id)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(body)
	}))
	defer srv.Close()

	arts, err := ScanArchive(context.Background(), &http.Client{},
		Archive{URL: srv.URL + "/a.tar.gz", Format: "tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].BuildID != id {
		t.Fatalf("got %+v, want one artifact with %s", arts, id)
	}
}
