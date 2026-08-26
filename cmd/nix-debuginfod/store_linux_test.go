//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeFetcher stands in for cache.nixos.org plus the NAR->tar->erofs pipeline: it
// packs a prepared directory into an erofs image. That keeps these tests about
// what the store does - what it fetches, when, and how often - rather than about
// the network.
type fakeFetcher struct {
	mu       sync.Mutex
	dirs     map[string]string // store path -> directory to pack
	archives map[string]string // archive path -> directory to pack
	metas    map[string]string // build ID -> archive path it lives in
	calls    []string          // every fetch, in order
}

func (f *fakeFetcher) record(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what)
}

func (f *fakeFetcher) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeFetcher) countOf(what string) int {
	n := 0
	for _, c := range f.log() {
		if c == what {
			n++
		}
	}
	return n
}

func pack(dir, out string) error {
	o, err := exec.Command("mkfs.erofs", "--quiet", "-zlz4hc", out, dir).CombinedOutput()
	if err != nil {
		return errors.New(string(o))
	}
	return nil
}

func (f *fakeFetcher) FetchNarByStorePath(_ context.Context, storePath, outFile string) error {
	f.record(storePath)
	f.mu.Lock()
	dir, ok := f.dirs[storePath]
	f.mu.Unlock()
	if !ok {
		return errors.New("fake: no such store path " + storePath)
	}
	return pack(dir, outFile)
}

func (f *fakeFetcher) DebuginfoMeta(_ context.Context, buildID string) (string, string, error) {
	f.record("meta:" + buildID)
	f.mu.Lock()
	archive, ok := f.metas[buildID]
	f.mu.Unlock()
	if !ok {
		return "", "", errors.New("fake: no such build id " + buildID)
	}
	return archive, memberPath(buildID), nil
}

func (f *fakeFetcher) FetchNarByArchive(_ context.Context, archive, outFile string) error {
	f.record("archive:" + archive)
	f.mu.Lock()
	dir, ok := f.archives[archive]
	f.mu.Unlock()
	if !ok {
		return errors.New("fake: no such archive " + archive)
	}
	return pack(dir, outFile)
}

// recorder behaves like a real server response, where an informational 1xx is
// sent and the response stays open. httptest.ResponseRecorder records 103 as the
// final status and then refuses the body with "response status code does not
// allow body" - a difference from production that would otherwise make every
// test using Early Hints fail for the wrong reason.
type recorder struct{ *httptest.ResponseRecorder }

func newRecorder() *recorder { return &recorder{httptest.NewRecorder()} }

func (r *recorder) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		return
	}
	r.ResponseRecorder.WriteHeader(code)
}

// memberPath is the newer of the two .build-id spellings. Tests that care about
// the older, suffix-less one say so explicitly.
func memberPath(buildID string) string {
	return filepath.Join("lib", "debug", ".build-id", buildID[:2], buildID[2:]+".debug")
}

// newFake builds an empty fetcher with every map ready.
func newFake() *fakeFetcher {
	return &fakeFetcher{
		dirs:     map[string]string{},
		archives: map[string]string{},
		metas:    map[string]string{},
	}
}

// addBuildID registers a build ID inside an archive, laying its debug file at
// the .build-id path memberFor computes. Extra files are merged into the same
// archive, which is how several build IDs come to share one.
func (f *fakeFetcher) addBuildID(t *testing.T, buildID, archive string, extra map[string]string) {
	t.Helper()
	files := map[string]string{memberPath(buildID): "DEBUG-" + buildID}
	for k, v := range extra {
		files[k] = v
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metas[buildID] = archive
	if prev, ok := f.archives[archive]; ok {
		// merge into the existing archive directory
		for k, v := range files {
			full := filepath.Join(prev, k)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if target, isLink := strings.CutPrefix(v, "->"); isLink {
				os.Remove(full)
				if err := os.Symlink(target, full); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.WriteFile(full, []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	f.archives[archive] = writeTree(t, files)
}

func newTestStore(t *testing.T, f *fakeFetcher) *store {
	t.Helper()
	requireMountable(t)
	root := t.TempDir()
	// A stand-in for /nix/store. The design needs store paths mounted where their
	// symlinks say they are; in a test that just means every link is written
	// against this root.
	mountRoot := filepath.Join(root, "store")
	for _, d := range []string{mountRoot, filepath.Join(root, "images"), filepath.Join(root, "entry")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := newStore(f, filepath.Join(root, "images"), mountRoot, filepath.Join(root, "entry"), 4)
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for point := range s.mounted {
			unmountErofs(point)
		}
	})
	return s
}

// writeTree materialises a directory from a path->content map. A content value
// prefixed with "->" becomes a symlink to the rest.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(content, "->"); ok {
			if err := os.Symlink(target, full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The property the whole design rests on: a chain of absolute symlinks across
// three separate images resolves by fetching one link at a time, and nothing but
// the links themselves says what to fetch next.
func TestStoreResolvesSymlinkChainLazily(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)

	pathA := filepath.Join(s.mountRoot, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a")
	pathB := filepath.Join(s.mountRoot, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b")
	pathC := filepath.Join(s.mountRoot, "cccccccccccccccccccccccccccccccc-c")

	f.dirs[pathA] = writeTree(t, map[string]string{"lib/hop": "->" + filepath.Join(pathB, "lib", "hop")})
	f.dirs[pathB] = writeTree(t, map[string]string{"lib/hop": "->" + filepath.Join(pathC, "lib", "real")})
	f.dirs[pathC] = writeTree(t, map[string]string{"lib/real": "THE-REAL-CONTENT"})

	if err := s.Ensure(context.Background(), pathA); err != nil {
		t.Fatalf("Ensure(A): %v", err)
	}
	if got := f.log(); len(got) != 1 {
		t.Fatalf("after Ensure(A) fetched %v; nothing else should have been touched yet", got)
	}

	rc, hops, err := s.Open(context.Background(), filepath.Join(pathA, "lib", "hop"))
	if err != nil {
		t.Fatalf("Open through the chain: %v", err)
	}
	if hops != 2 {
		t.Errorf("hops = %d, want 2 (B then C)", hops)
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "THE-REAL-CONTENT" {
		t.Errorf("content = %q, want THE-REAL-CONTENT", body)
	}

	want := []string{pathA, pathB, pathC}
	got := f.log()
	if len(got) != len(want) {
		t.Fatalf("fetches = %v, want exactly %v - one per hop, none speculative", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fetch %d = %s, want %s (order matters: each link names the next)", i, got[i], want[i])
		}
	}
}

// A cold store path is a NAR download plus a mkfs.erofs run. Two requests that
// need the same one must produce one fetch, not two.
func TestStoreCoalescesConcurrentEnsure(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)

	path := filepath.Join(s.mountRoot, "dddddddddddddddddddddddddddddddd-d")
	f.dirs[path] = writeTree(t, map[string]string{"f": "x"})

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Ensure(context.Background(), path)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure #%d: %v", i, err)
		}
	}
	if n := f.countOf(path); n != 1 {
		t.Errorf("fetched %d times, want 1 - concurrent callers must coalesce", n)
	}
}

// A file that is simply not there must fail, not send the store hunting.
func TestMissingFileDoesNotTriggerFetches(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)

	path := filepath.Join(s.mountRoot, "gggggggggggggggggggggggggggggggg-e")
	f.dirs[path] = writeTree(t, map[string]string{"present": "x"})
	if err := s.Ensure(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.Open(context.Background(), filepath.Join(path, "absent"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
	if n := len(f.log()); n != 1 {
		t.Errorf("%d fetches for a missing file, want just the initial Ensure", n)
	}
}

// A symlink pointing outside the store is not ours to chase.
func TestSymlinkOutsideStoreIsNotFetched(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)

	path := filepath.Join(s.mountRoot, "ffffffffffffffffffffffffffffffff-f")
	f.dirs[path] = writeTree(t, map[string]string{"out": "->/etc/definitely-not-here"})
	if err := s.Ensure(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Open(context.Background(), filepath.Join(path, "out")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
	if n := len(f.log()); n != 1 {
		t.Errorf("%d fetches, want just the initial Ensure", n)
	}
}

// End to end: the member named by the debuginfo endpoint is itself a symlink into
// another store path, which is the shape nix actually produces.
func TestGetDebuginfoFollowsMemberSymlink(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)

	const buildID = "0123456789abcdef0123456789abcdef01234567"
	target := filepath.Join(s.mountRoot, "99999999999999999999999999999999-lib")

	f.dirs[target] = writeTree(t, map[string]string{"lib/debug/real.debug": "DEBUG-BYTES"})
	f.addBuildID(t, buildID, "nar/"+strings.Repeat("c", 52)+".nar.xz", map[string]string{
		memberPath(buildID): "->" + filepath.Join(target, "lib", "debug", "real.debug"),
	})

	out := newRecorder()
	if err := s.GetDebuginfo(context.Background(), buildID, out); err != nil {
		t.Fatalf("GetDebuginfo: %v", err)
	}
	if out.Body.String() != "DEBUG-BYTES" {
		t.Errorf("served %q, want DEBUG-BYTES", out.Body.String())
	}

	// Second call must serve from what is already mounted.
	before := len(f.log())
	out = newRecorder()
	if err := s.GetDebuginfo(context.Background(), buildID, out); err != nil {
		t.Fatalf("second GetDebuginfo: %v", err)
	}
	if n := len(f.log()); n != before {
		t.Errorf("second request fetched again (%d -> %d)", before, n)
	}
	if out.Body.String() != "DEBUG-BYTES" {
		t.Errorf("second call served %q", out.Body.String())
	}
}

// The -debug NAR names the unstripped binary with a sibling symlink, so the
// executable endpoint is a link-follow into the package's main output. This is
// the first endpoint that genuinely exercises the lazy resolution: unlike
// debuginfo, the answer is never in the image we started from.
func TestGetExecutableFollowsSiblingSymlink(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)

	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd077"
	member := "lib/debug/.build-id/8a/e0b698f2d4e727f569f64bb166e08ae30bd077.debug"
	base := strings.TrimSuffix(member, ".debug")

	mainOut := filepath.Join(s.mountRoot, "57iz36553175g3178pvxjij8z5rcsd4n-glibc-2.42-61")
	f.dirs[mainOut] = writeTree(t, map[string]string{"lib/gconv/IBM1142.so": "ELF-BINARY"})

	// Exactly the shape cache.nixos.org serves: the debug file plus three
	// siblings, of which .executable points into another store path.
	f.addBuildID(t, buildID, "nar/"+strings.Repeat("b", 52)+".nar.xz", map[string]string{
		base + ".executable":          "->" + filepath.Join(mainOut, "lib", "gconv", "IBM1142.so"),
		base + ".source":              "->" + filepath.Join(s.mountRoot, "6zxixmszg0vbhpaa8rxwfr05by9axj68-glibc-2.42.tar.xz"),
		base + ".sourceoverlay":       "->../../../../src/overlay",
		"src/overlay/glibc-2.42/keep": "x",
	})

	out := newRecorder()
	if err := s.GetExecutable(context.Background(), buildID, out); err != nil {
		t.Fatalf("GetExecutable: %v", err)
	}
	if out.Body.String() != "ELF-BINARY" {
		t.Errorf("served %q, want ELF-BINARY", out.Body.String())
	}

	// The main output had to be fetched; the source tarball must not have been.
	if n := f.countOf(mainOut); n != 1 {
		t.Errorf("main output fetched %d times, want 1", n)
	}
	for _, c := range f.log() {
		if strings.Contains(c, "glibc-2.42.tar.xz") {
			t.Errorf("fetched the source tarball for an executable request: %s", c)
		}
	}

	// Debuginfo for the same build ID must still come out of the entry image
	// alone, with no further fetches.
	before := len(f.log())
	out = newRecorder()
	if err := s.GetDebuginfo(context.Background(), buildID, out); err != nil {
		t.Fatalf("GetDebuginfo: %v", err)
	}
	if out.Body.String() != "DEBUG-"+buildID {
		t.Errorf("debuginfo served %q", out.Body.String())
	}
	if n := len(f.log()); n != before {
		t.Errorf("debuginfo triggered %d extra fetches; it is self-contained", n-before)
	}
}

// sourceFixture builds the shape cache.nixos.org actually serves: a -debug entry
// image with the four .build-id entries, an overlay inside it, and a separate
// store path holding the pristine sources.
func sourceFixture(t *testing.T, s *store, f *fakeFetcher, buildID string, overlay, pristine map[string]string) (member, srcPath string) {
	t.Helper()
	member = memberPath(buildID)
	base := strings.TrimSuffix(member, ".debug")
	srcPath = filepath.Join(s.mountRoot, "6zxixmszg0vbhpaa8rxwfr05by9axj68-glibc-2.42")

	extra := map[string]string{
		base + ".source":        "->" + srcPath,
		base + ".sourceoverlay": "->../../../../src/overlay",
	}
	for name, content := range overlay {
		extra[filepath.Join("src/overlay", name)] = content
	}
	f.addBuildID(t, buildID, "nar/"+strings.Repeat("a", 52)+".nar.xz", extra)
	f.dirs[srcPath] = writeTree(t, pristine)
	return member, srcPath
}

// The overlay holds what the build actually compiled - patched and generated
// files. Serving the pristine copy where an overlay exists would hand back
// source that does not match the binary, so the order is load-bearing.
func TestGetSourcePrefersOverlayOverPristine(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "e0b698f2d4e727f569f64bb166e08ae30bd077aa"

	_, srcPath := sourceFixture(t, s, f, buildID,
		map[string]string{"glibc-2.42/elf/dl-find_object.c": "PATCHED"},
		map[string]string{"glibc-2.42/elf/dl-find_object.c": "PRISTINE"})

	out := newRecorder()
	if err := s.GetSource(context.Background(), buildID, "/build/glibc-2.42/elf/dl-find_object.c", out); err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if out.Body.String() != "PATCHED" {
		t.Errorf("served %q, want PATCHED - the overlay must win", out.Body.String())
	}
	// The source tree IS fetched even though the overlay answered: the index
	// scores both together, which is what stops a weak overlay match from
	// beating a strong one in .source, and scoring needs both mounted. The cost
	// is small in practice - the overlay only holds patched and generated files,
	// so nearly every other request needs .source anyway.
	if n := f.countOf(srcPath); n != 1 {
		t.Errorf("source tree fetched %d times, want 1", n)
	}
}

func TestGetSourceFallsBackToPristine(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "aab698f2d4e727f569f64bb166e08ae30bd077bb"

	_, srcPath := sourceFixture(t, s, f, buildID,
		map[string]string{"glibc-2.42/only-in-overlay.c": "X"},
		map[string]string{"glibc-2.42/elf/dl-find_object.c": "PRISTINE"})

	out := newRecorder()
	if err := s.GetSource(context.Background(), buildID, "/build/glibc-2.42/elf/dl-find_object.c", out); err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if out.Body.String() != "PRISTINE" {
		t.Errorf("served %q, want PRISTINE", out.Body.String())
	}
	if n := f.countOf(srcPath); n != 1 {
		t.Errorf("source store path fetched %d times, want 1", n)
	}
}

// The compiler's path carries a sandbox prefix that exists nowhere on disk, and
// how many components to strip differs per package. Longest-suffix-first has to
// find the file without being told.
func TestGetSourceStripsBuildPrefix(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "bbc698f2d4e727f569f64bb166e08ae30bd077cc"

	sourceFixture(t, s, f, buildID,
		map[string]string{},
		map[string]string{"elf/dl-find_object.c": "FOUND"})

	out := newRecorder()
	err := s.GetSource(context.Background(), buildID,
		"/build/some/deep/sandbox/elf/dl-find_object.c", out)
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if out.Body.String() != "FOUND" {
		t.Errorf("served %q, want FOUND", out.Body.String())
	}
}

// The requested path comes from the client. Joining it onto a mounted root is
// exactly where a traversal would escape into the host filesystem.
func TestGetSourceRejectsTraversal(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "ccd698f2d4e727f569f64bb166e08ae30bd077dd"

	sourceFixture(t, s, f, buildID, map[string]string{}, map[string]string{"x.c": "OK"})

	for _, bad := range []string{
		"/../../../../etc/passwd",
		"/build/../../../../../etc/hostname",
	} {
		out := newRecorder()
		err := s.GetSource(context.Background(), buildID, bad, out)
		if err == nil {
			t.Errorf("%s: served %d bytes, want an error", bad, out.Body.Len())
		}
		if strings.Contains(out.Body.String(), "root:") {
			t.Fatalf("%s: escaped the mount and read a host file", bad)
		}
	}
}

// A compiler sometimes records an absolute store path rather than a sandbox one:
// headers from a dependency, or a file compiled straight out of the store. That
// file is in neither the overlay nor .source and needs its own NAR.
func TestGetSourceFetchesStorePathNamedInTheRequest(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "dde698f2d4e727f569f64bb166e08ae30bd077ee"

	// The overlay deliberately contains a file at the same *suffix*. If the
	// store-path branch fell through to suffix matching, this is what would come
	// back - the wrong package's header, with nothing to signal it.
	sourceFixture(t, s, f, buildID,
		map[string]string{"include/stdio.h": "WRONG-PACKAGE-HEADER"},
		map[string]string{"placeholder.c": "x"})

	dep := filepath.Join(s.mountRoot, "57iz36553175g3178pvxjij8z5rcsd4n-glibc-dev")
	f.dirs[dep] = writeTree(t, map[string]string{"include/stdio.h": "REAL-GLIBC-HEADER"})

	out := newRecorder()
	req := filepath.Join(dep, "include", "stdio.h")
	if err := s.GetSource(context.Background(), buildID, req, out); err != nil {
		t.Fatalf("GetSource(%s): %v", req, err)
	}
	if out.Body.String() != "REAL-GLIBC-HEADER" {
		t.Errorf("served %q, want REAL-GLIBC-HEADER - an exact store path must not fall through to suffix matching", out.Body.String())
	}
	if n := f.countOf(dep); n != 1 {
		t.Errorf("dependency store path fetched %d times, want 1", n)
	}
}

// A source request can name a store path, so this runs on client input: every
// accepted one costs a narinfo fetch. Anything not shaped like a nix hash must
// be rejected before it becomes upstream traffic.
func TestStorePathHashIsValidated(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)

	good := "57iz36553175g3178pvxjij8z5rcsd4n-glibc-2.42-61"
	if got := s.storePathOf(filepath.Join(s.mountRoot, good, "lib", "x.so")); got == "" {
		t.Errorf("a real store path was rejected")
	}

	for _, bad := range []string{
		"tooshort-name",
		"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-name",  // e is not in the nix alphabet
		"57iz36553175g3178pvxjij8z5rcsd4-name",   // 31 chars
		"57iz36553175g3178pvxjij8z5rcsd4nn-name", // 33
		"nodashhere57iz36553175g3178pvxji",
		"../../etc",
	} {
		if got := s.storePathOf(filepath.Join(s.mountRoot, bad, "x")); got != "" {
			t.Errorf("storePathOf accepted %q -> %q", bad, got)
		}
	}
	// Nothing outside the mount root is ours.
	if got := s.storePathOf("/etc/passwd"); got != "" {
		t.Errorf("accepted a path outside the store: %q", got)
	}
}

// A DWARF path routinely carries ".." - a comp_dir of /build/pkg/build plus a
// DW_AT_name of ../src/main.c is ordinary, not an attack. GetSource cleans the
// request before anything looks at it, so these resolve lexically and then match
// by suffix like any other path.
func TestGetSourceHandlesDotDot(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "1234698f2d4e727f569f64bb166e08ae30bd0711"

	sourceFixture(t, s, f, buildID, map[string]string{}, map[string]string{
		"bar/main.c":       "BAR",
		"build/bar/main.c": "BUILD-BAR",
	})

	for _, tc := range []struct{ name, req, want string }{
		{"no dotdot", "/build/bar/main.c", "BUILD-BAR"},
		{"dotdot in the middle", "/build/foo/../bar/main.c", "BUILD-BAR"},
		{"comp_dir plus ../", "/build/pkg/build/../bar/main.c", "BAR"},
		{"leading dotdot", "/../build/bar/main.c", "BUILD-BAR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := newRecorder()
			if err := s.GetSource(context.Background(), buildID, tc.req, out); err != nil {
				t.Fatalf("GetSource(%s): %v", tc.req, err)
			}
			if out.Body.String() != tc.want {
				t.Errorf("%s served %q, want %q", tc.req, out.Body.String(), tc.want)
			}
		})
	}
}

// ".." between two store paths must land on the second one, and must not fetch
// the first - the request never actually referred to it.
func TestGetSourceDotDotBetweenStorePaths(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "1234698f2d4e727f569f64bb166e08ae30bd0722"
	sourceFixture(t, s, f, buildID, map[string]string{}, map[string]string{"z.c": "P"})

	first := filepath.Join(s.mountRoot, "6zxixmszg0vbhpaa8rxwfr05by9axj68-first")
	other := filepath.Join(s.mountRoot, "57iz36553175g3178pvxjij8z5rcsd4n-other")
	f.dirs[first] = writeTree(t, map[string]string{"y.c": "FIRST-Y"})
	f.dirs[other] = writeTree(t, map[string]string{"x.c": "OTHER-X"})

	// Built by concatenation: filepath.Join would clean it here and the test
	// would never exercise the ".." at all.
	raw := first + "/../" + filepath.Base(other) + "/x.c"

	out := newRecorder()
	if err := s.GetSource(context.Background(), buildID, raw, out); err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if out.Body.String() != "OTHER-X" {
		t.Errorf("served %q, want OTHER-X", out.Body.String())
	}
	if n := f.countOf(first); n != 0 {
		t.Errorf("fetched the store path the request only passed through (%d times)", n)
	}
}

// The same cleaning that makes ".." work is what keeps a request from climbing
// out of a mounted store path into the host filesystem. A store path prefix
// followed by enough ".." collapses to a path that is no longer a store path,
// and the exact branch declines it.
func TestGetSourceStorePathCannotEscape(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "1234698f2d4e727f569f64bb166e08ae30bd0733"
	sourceFixture(t, s, f, buildID, map[string]string{}, map[string]string{"z.c": "P"})

	sp := filepath.Join(s.mountRoot, "57iz36553175g3178pvxjij8z5rcsd4n-pkg")
	f.dirs[sp] = writeTree(t, map[string]string{"lib/a": "x"})
	if err := s.Ensure(context.Background(), sp); err != nil {
		t.Fatal(err)
	}

	out := newRecorder()
	err := s.GetSource(context.Background(), buildID,
		sp+"/../../../../../../../../etc/passwd", out)
	if err == nil {
		t.Errorf("served %d bytes for a path outside the store", out.Body.Len())
	}
	if strings.Contains(out.Body.String(), "root:") {
		t.Fatal("escaped the mount and read a host file")
	}
}

// A tarball unpacks into a directory of its own, and the compiler's path knows
// nothing about that level. Trimming suffixes from the request cannot bridge it,
// because the extra component is on the tree side.
//
// This is the shape that failed in production: glibc sources live at
// glibc-2.42/sysdeps/unix/sysv/linux/getsysstats.c while the request, after ".."
// is resolved, is /sysdeps/unix/sysv/linux/getsysstats.c.
func TestGetSourceLooksInsideTheTarballDirectory(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd077"

	sourceFixture(t, s, f, buildID,
		map[string]string{"glibc-2.42/misc/patched.c": "OVERLAY"},
		map[string]string{
			"glibc-2.42/sysdeps/unix/sysv/linux/getsysstats.c": "PRISTINE-SYSDEPS",
			"glibc-2.42/misc/getsysstats.c":                    "PRISTINE-MISC",
		})

	for _, tc := range []struct{ name, req, want string }{
		{"the production case", "/build/../sysdeps/unix/sysv/linux/getsysstats.c", "PRISTINE-SYSDEPS"},
		{"path already carrying the package dir", "/build/glibc-2.42/misc/getsysstats.c", "PRISTINE-MISC"},
		{"overlay is still reached one level down", "/build/../misc/patched.c", "OVERLAY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := newRecorder()
			if err := s.GetSource(context.Background(), buildID, tc.req, out); err != nil {
				t.Fatalf("GetSource(%s): %v", tc.req, err)
			}
			if out.Body.String() != tc.want {
				t.Errorf("%s served %q, want %q", tc.req, out.Body.String(), tc.want)
			}
		})
	}
}

// A long suffix found one level down beats a short one at the top: two files
// share a basename, and only the fuller path says which was asked for.
func TestGetSourcePrefersTheLongerMatchOverADepthPreference(t *testing.T) {
	f := newFake()
	s := newTestStore(t, f)
	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd088"

	sourceFixture(t, s, f, buildID, map[string]string{}, map[string]string{
		"getsysstats.c": "SHALLOW-WRONG",
		"glibc-2.42/sysdeps/unix/sysv/linux/getsysstats.c": "DEEP-RIGHT",
	})

	out := newRecorder()
	if err := s.GetSource(context.Background(), buildID,
		"/build/../sysdeps/unix/sysv/linux/getsysstats.c", out); err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if out.Body.String() != "DEEP-RIGHT" {
		t.Errorf("served %q, want DEEP-RIGHT", out.Body.String())
	}
}

// A request that names a store path directly, with nothing mounted yet and the
// mount root not even present. This 404'd in production without attempting a
// single fetch: missingFor walks component by component and stopped at "/nix",
// which is not a store path, so it never reached the one it could have fetched.
func TestGetSourceStorePathWorksOnAColdStore(t *testing.T) {
	requireMountable(t)

	f := newFake()
	root := t.TempDir()
	// Deliberately NOT created here - newStore has to do it.
	mountRoot := filepath.Join(root, "deep", "nix", "store")
	s := newStore(f, filepath.Join(root, "images"), mountRoot, filepath.Join(root, "entry"), 4)
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for point := range s.mounted {
			unmountErofs(point)
		}
	})

	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd099"
	f.addBuildID(t, buildID, "nar/"+strings.Repeat("d", 52)+".nar.xz", nil)

	dep := filepath.Join(mountRoot, "57iz36553175g3178pvxjij8z5rcsd4n-glibc")
	f.dirs[dep] = writeTree(t, map[string]string{"lib/gconv/IBM1142.so": "ELF"})

	out := newRecorder()
	req := filepath.Join(dep, "lib", "gconv", "IBM1142.so")
	if err := s.GetSource(context.Background(), buildID, req, out); err != nil {
		t.Fatalf("GetSource(%s): %v", req, err)
	}
	if out.Body.String() != "ELF" {
		t.Errorf("served %q, want ELF", out.Body.String())
	}
	if n := f.countOf(dep); n != 1 {
		t.Errorf("store path fetched %d times, want 1", n)
	}
}

// The exact production failure: everything works, the container is restarted,
// and the same request 404s from then on.
//
// Mounts do not survive a restart - a new mount namespace - but the empty
// DIRECTORY each mount point left behind does. The lazy path keys on ENOENT, so
// an empty leftover makes os.Open succeed, nothing is re-mounted, and the index
// walks an empty tree. Images and mount points both live in the persisted cache
// volume, which is what the shared root here stands in for.
func TestRestartWithLeftoverMountPointsStillServes(t *testing.T) {
	requireMountable(t)

	root := t.TempDir() // survives the "restart", like the cache volume
	mountRoot := filepath.Join(root, "nix", "store")
	entryRoot := filepath.Join(root, "entry")
	imageDir := filepath.Join(root, "images")

	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd0aa"
	base := strings.TrimSuffix(memberPath(buildID), ".debug")
	srcPath := filepath.Join(mountRoot, "6zxixmszg0vbhpaa8rxwfr05by9axj68-glibc-2.42")

	newFetcher := func() *fakeFetcher {
		f := newFake()
		f.addBuildID(t, buildID, "nar/"+strings.Repeat("e", 52)+".nar.xz", map[string]string{
			base + ".source":        "->" + srcPath,
			base + ".sourceoverlay": "->../../../../src/overlay",
			"src/overlay/keep":      "x",
		})
		f.dirs[srcPath] = writeTree(t, map[string]string{"glibc-2.42/misc/getsysstats.c": "MISC"})
		return f
	}

	serve := func(f *fakeFetcher) (*store, string, error) {
		s := newStore(f, imageDir, mountRoot, entryRoot, 4)
		out := newRecorder()
		err := s.GetSource(context.Background(), buildID, "/getsysstats.c", out)
		return s, out.Body.String(), err
	}

	first := newFetcher()
	s1, got, err := serve(first)
	if err != nil || got != "MISC" {
		t.Fatalf("before restart: %q, %v", got, err)
	}

	// The restart. Everything the first process mounted goes away - a container
	// restart builds a fresh mount namespace - while the DIRECTORIES it created
	// stay, because they live in the persisted cache volume. Unmounting by hand
	// here is what makes this a restart rather than a second store sharing the
	// first one's live mounts; without it the test passes either way and proves
	// nothing.
	s1.mu.Lock()
	for point := range s1.mounted {
		if err := unmountErofs(point); err != nil {
			t.Fatalf("unmount %s: %v", point, err)
		}
	}
	s1.mu.Unlock()

	if fi, err := os.Stat(srcPath); err != nil || !fi.IsDir() {
		t.Fatalf("the first run should have left an empty mount point behind: %v", err)
	}
	if ents, err := os.ReadDir(srcPath); err != nil || len(ents) != 0 {
		t.Fatalf("the leftover mount point should be empty: %d entries, %v", len(ents), err)
	}

	second := newFetcher()
	s2, got, err := serve(second)
	if err != nil {
		t.Fatalf("after restart: %v", err)
	}
	if got != "MISC" {
		t.Errorf("after restart served %q, want MISC", got)
	}
	// The images are still on disk, so nothing should have been downloaded again;
	// only the mounts are rebuilt.
	if n := second.countOf(srcPath); n != 0 {
		t.Errorf("re-fetched the source tree %d times after a restart; the image was already cached", n)
	}

	s2.mu.Lock()
	defer s2.mu.Unlock()
	for point := range s2.mounted {
		unmountErofs(point)
	}
}

// The point of keying by archive: /debuginfo/<buildid> hands back an archive
// that many build IDs share - one glibc NAR covers 389 of them - so the
// expensive half must run once, not once per build ID.
func TestArchiveSharedByManyBuildIDsIsFetchedOnce(t *testing.T) {
	requireMountable(t)

	f := newFake()
	s := newTestStore(t, f)

	archive := "nar/" + strings.Repeat("f", 52) + ".nar.xz"
	ids := []string{
		"00117ac285340caaaf796d4400b688aff7326f3e",
		"8ae0b698f2d4e727f569f64bb166e08ae30bd077",
		"01999718a774b83c44cb041f0e05283f9c15c668",
	}
	for _, id := range ids {
		f.addBuildID(t, id, archive, nil)
	}

	for _, id := range ids {
		out := newRecorder()
		if err := s.GetDebuginfo(context.Background(), id, out); err != nil {
			t.Fatalf("GetDebuginfo(%s): %v", id, err)
		}
		if out.Body.String() != "DEBUG-"+id {
			t.Errorf("%s served %q", id, out.Body.String())
		}
	}

	if n := f.countOf("archive:" + archive); n != 1 {
		t.Errorf("archive downloaded %d times for %d build IDs, want 1", n, len(ids))
	}
	// The cheap half still runs per build ID, but only the first time: after
	// that the symlink answers.
	for _, id := range ids {
		if n := f.countOf("meta:" + id); n != 1 {
			t.Errorf("metadata for %s looked up %d times, want 1", id, n)
		}
	}
}

// A second store over the same directories - a restart - must read the archive
// back out of the symlink instead of asking the network again.
func TestArchiveLinkSurvivesRestart(t *testing.T) {
	requireMountable(t)

	root := t.TempDir()
	mountRoot := filepath.Join(root, "nix", "store")
	entryRoot := filepath.Join(root, "entry")
	imageDir := filepath.Join(root, "images")
	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd0bb"
	archive := "nar/" + strings.Repeat("g", 52) + ".nar.xz"

	run := func() (*store, *fakeFetcher, string) {
		f := newFake()
		f.addBuildID(t, buildID, archive, nil)
		s := newStore(f, imageDir, mountRoot, entryRoot, 4)
		out := newRecorder()
		if err := s.GetDebuginfo(context.Background(), buildID, out); err != nil {
			t.Fatalf("GetDebuginfo: %v", err)
		}
		return s, f, out.Body.String()
	}

	s1, _, got := run()
	if got != "DEBUG-"+buildID {
		t.Fatalf("before restart: %q", got)
	}
	s1.mu.Lock()
	for point := range s1.mounted {
		unmountErofs(point)
	}
	s1.mu.Unlock()

	s2, f2, got := run()
	if got != "DEBUG-"+buildID {
		t.Errorf("after restart: %q", got)
	}
	if n := f2.countOf("meta:" + buildID); n != 0 {
		t.Errorf("looked the build ID up again after a restart (%d times); the symlink records it", n)
	}
	if n := f2.countOf("archive:" + archive); n != 0 {
		t.Errorf("re-downloaded the archive after a restart (%d times); the image was cached", n)
	}
	s2.mu.Lock()
	defer s2.mu.Unlock()
	for point := range s2.mounted {
		unmountErofs(point)
	}
}

// The debuginfod protocol headers have to carry real values, and be set before
// the body: once the status is committed the header block is closed.
func TestResponseHeadersDescribeWhatIsServed(t *testing.T) {
	requireMountable(t)

	f := newFake()
	s := newTestStore(t, f)

	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd0cc"
	base := strings.TrimSuffix(memberPath(buildID), ".debug")
	archive := "nar/" + strings.Repeat("h", 52) + ".nar.xz"

	mainOut := filepath.Join(s.mountRoot, "57iz36553175g3178pvxjij8z5rcsd4n-glibc-2.42-61")
	binary := filepath.Join(mainOut, "lib", "gconv", "IBM1142.so")
	f.dirs[mainOut] = writeTree(t, map[string]string{"lib/gconv/IBM1142.so": "ELF-BINARY-BYTES"})
	f.addBuildID(t, buildID, archive, map[string]string{
		base + ".executable": "->" + binary,
	})

	t.Run("debuginfo", func(t *testing.T) {
		out := newRecorder()
		if err := s.GetDebuginfo(context.Background(), buildID, out); err != nil {
			t.Fatal(err)
		}
		h := out.Header()
		// Size is the payload's, taken from the file itself so it cannot drift.
		if got, want := h.Get("x-debuginfod-size"), strconv.Itoa(out.Body.Len()); got != want {
			t.Errorf("size = %q, want %q (the bytes actually sent)", got, want)
		}
		// For debug info, "file" names the binary it describes - the .executable
		// sibling's target, not the member inside the archive.
		if got := h.Get("x-debuginfod-file"); got != binary {
			t.Errorf("file = %q, want the .executable target %q", got, binary)
		}
		if got := h.Get("x-debuginfod-archive"); got != archive {
			t.Errorf("archive = %q, want %q", got, archive)
		}
	})

	t.Run("executable", func(t *testing.T) {
		out := newRecorder()
		if err := s.GetExecutable(context.Background(), buildID, out); err != nil {
			t.Fatal(err)
		}
		if got, want := out.Header().Get("x-debuginfod-size"), strconv.Itoa(len("ELF-BINARY-BYTES")); got != want {
			t.Errorf("size = %q, want %q", got, want)
		}
		if got := out.Header().Get("x-debuginfod-file"); got != binary {
			t.Errorf("file = %q, want %q", got, binary)
		}
	})
}

// The archive must be reported on a warm request too, where the answer comes
// from the symlink and no lookup happens. It cannot be rebuilt from the key -
// the extension is not ours to guess - so it is remembered once per archive.
func TestArchiveHeaderSurvivesRestart(t *testing.T) {
	requireMountable(t)

	root := t.TempDir()
	mountRoot := filepath.Join(root, "nix", "store")
	entryRoot := filepath.Join(root, "entry")
	imageDir := filepath.Join(root, "images")
	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd0dd"
	archive := "nar/" + strings.Repeat("j", 52) + ".nar.zst" // deliberately not .xz

	run := func() (*store, string) {
		f := newFake()
		f.addBuildID(t, buildID, archive, nil)
		s := newStore(f, imageDir, mountRoot, entryRoot, 4)
		out := newRecorder()
		if err := s.GetDebuginfo(context.Background(), buildID, out); err != nil {
			t.Fatalf("GetDebuginfo: %v", err)
		}
		return s, out.Header().Get("x-debuginfod-archive")
	}

	s1, got := run()
	if got != archive {
		t.Fatalf("cold request: archive = %q, want %q", got, archive)
	}
	s1.mu.Lock()
	for point := range s1.mounted {
		unmountErofs(point)
	}
	s1.mu.Unlock()

	s2, got := run()
	if got != archive {
		t.Errorf("warm request: archive = %q, want %q - reconstructing it from the key would have guessed .nar.xz", got, archive)
	}
	s2.mu.Lock()
	defer s2.mu.Unlock()
	for point := range s2.mounted {
		unmountErofs(point)
	}
}

// A source file comes from one of two places and they are not interchangeable:
// the overlay lives inside the -debug NAR, the pristine tree in a store path of
// its own. The archive header has to name whichever actually answered.
func TestSourceHeadersNameTheTreeThatAnswered(t *testing.T) {
	requireMountable(t)

	f := newFake()
	s := newTestStore(t, f)
	const buildID = "8ae0b698f2d4e727f569f64bb166e08ae30bd0ee"

	_, srcPath := sourceFixture(t, s, f, buildID,
		map[string]string{"glibc-2.42/misc/patched.c": "OVERLAY"},
		map[string]string{"glibc-2.42/elf/pristine.c": "PRISTINE"})

	t.Run("from the overlay", func(t *testing.T) {
		out := newRecorder()
		if err := s.GetSource(context.Background(), buildID, "/build/glibc-2.42/misc/patched.c", out); err != nil {
			t.Fatal(err)
		}
		if got := out.Body.String(); got != "OVERLAY" {
			t.Fatalf("served %q", got)
		}
		// The overlay is part of the -debug NAR, so that is the archive.
		if got := out.Header().Get("x-debuginfod-archive"); !strings.HasPrefix(got, "nar/") {
			t.Errorf("archive = %q, want the -debug NAR", got)
		}
		if got := out.Header().Get("x-debuginfod-file"); got != "glibc-2.42/misc/patched.c" {
			t.Errorf("file = %q, want it relative to the overlay", got)
		}
	})

	t.Run("from the pristine tree", func(t *testing.T) {
		out := newRecorder()
		if err := s.GetSource(context.Background(), buildID, "/build/glibc-2.42/elf/pristine.c", out); err != nil {
			t.Fatal(err)
		}
		if got := out.Body.String(); got != "PRISTINE" {
			t.Fatalf("served %q", got)
		}
		// Here the archive is the store path the sources came from - read
		// through the symlink, because the symlink's own path is an internal
		// detail and identifies nothing.
		if got := out.Header().Get("x-debuginfod-archive"); got != srcPath {
			t.Errorf("archive = %q, want the .source target %q", got, srcPath)
		}
		if got := out.Header().Get("x-debuginfod-file"); got != "glibc-2.42/elf/pristine.c" {
			t.Errorf("file = %q, want it relative to the source tree", got)
		}
		// Nothing internal may leak: the mount root is ours, not the client's.
		for _, h := range []string{"x-debuginfod-file"} {
			if strings.HasPrefix(out.Header().Get(h), s.entryRoot) {
				t.Errorf("%s exposes the entry mount point: %q", h, out.Header().Get(h))
			}
		}
	})
}
