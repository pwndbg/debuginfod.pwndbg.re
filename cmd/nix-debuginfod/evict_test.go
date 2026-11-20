package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pwndbg/debuginfod.pwndbg.re/srcindex"
)

// evictStore builds a store over a temp directory, with unmounting stubbed so
// the selection logic can be tested anywhere rather than only on a Linux box
// with the privileges to mount.
func evictStore(t *testing.T) (*store, *[]string) {
	t.Helper()
	root := t.TempDir()
	var unmounted []string
	var mu sync.Mutex
	s := &store{
		imageDir:  filepath.Join(root, "images"),
		mountRoot: filepath.Join(root, "nix", "store"),
		entryRoot: filepath.Join(root, "entry"),
		mounted:   map[string]mountKind{},
		idx:       map[string]*srcindex.Index{},
		used:      map[string]time.Time{},
		unmount: func(target string) error {
			mu.Lock()
			unmounted = append(unmounted, target)
			mu.Unlock()
			return nil
		},
	}
	for _, d := range []string{s.imageDir, s.mountRoot, s.entryRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s, &unmounted
}

func writeImage(t *testing.T, s *store, name string, size int, used time.Time) string {
	t.Helper()
	p := filepath.Join(s.imageDir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if !used.IsZero() {
		s.used[p] = used
	}
	return p
}

func imagesOnDisk(t *testing.T, s *store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.imageDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".erofs") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestEvictKeepsEverythingWhenUnderBudget(t *testing.T) {
	s, _ := evictStore(t)
	writeImage(t, s, "aaa.erofs", 100, time.Now())
	writeImage(t, s, "bbb.erofs", 100, time.Now())

	if err := s.evictOnce(1000, 100); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	if got := imagesOnDisk(t, s); len(got) != 2 {
		t.Errorf("images = %v, want both kept", got)
	}
}

// Ordering is by last use, not by download time: an image fetched long ago but
// still being served must outlive one fetched recently and never touched again.
func TestEvictDropsLeastRecentlyUsedImages(t *testing.T) {
	s, _ := evictStore(t)
	now := time.Now()
	writeImage(t, s, "busy.erofs", 400, now)
	writeImage(t, s, "idle.erofs", 400, now.Add(-48*time.Hour))
	writeImage(t, s, "warm.erofs", 400, now.Add(-1*time.Hour))

	if err := s.evictOnce(800, 100); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	got := imagesOnDisk(t, s)
	if len(got) != 2 || got[0] != "busy.erofs" || got[1] != "warm.erofs" {
		t.Errorf("images = %v, want busy and warm", got)
	}
}

// The .name sidecars and the buildid symlinks are the memory of which NAR a key
// came from. They are a few hundred bytes and keeping them means a re-request
// rebuilds the image without another metadata round trip, so eviction must free
// the expensive bytes without discarding the cheap knowledge.
func TestEvictKeepsArchiveNamesAndBuildIDLinks(t *testing.T) {
	s, _ := evictStore(t)
	writeImage(t, s, "archive-abc.erofs", 900, time.Now().Add(-72*time.Hour))

	sidecar := filepath.Join(s.imageDir, "archive-abc.name")
	if err := os.WriteFile(sidecar, []byte("nar/abc.nar.zst"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(s.imageDir, "buildid")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "deadbeef")
	if err := os.Symlink("../archive-abc.erofs", link); err != nil {
		t.Fatal(err)
	}

	if err := s.evictOnce(100, 100); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	if got := imagesOnDisk(t, s); len(got) != 0 {
		t.Errorf("image survived eviction: %v", got)
	}
	if _, err := os.ReadFile(sidecar); err != nil {
		t.Errorf("the archive name sidecar was deleted: %v", err)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Errorf("the build id link was deleted: %v", err)
	}
}

// Mounts are a separate resource from disk: each one costs kernel memory and
// counts against fs.mount-max, so a process can be well inside its byte budget
// and still be the reason nothing on the host can mount anything.
func TestEvictTrimsMountsIndependentlyOfBytes(t *testing.T) {
	s, unmounted := evictStore(t)
	now := time.Now()
	for i, name := range []string{"aaa-one", "bbb-two", "ccc-three"} {
		point := filepath.Join(s.mountRoot, name)
		if err := os.MkdirAll(point, 0o755); err != nil {
			t.Fatal(err)
		}
		s.mounted[point] = mountFileBacked
		hash, _, _ := strings.Cut(name, "-")
		writeImage(t, s, hash+".erofs", 10, now.Add(-time.Duration(i)*time.Hour))
	}

	// Far under the byte budget: only the mount limit should bite.
	if err := s.evictOnce(1<<30, 2); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	if len(*unmounted) != 1 {
		t.Fatalf("unmounted %v, want exactly one", *unmounted)
	}
	if filepath.Base((*unmounted)[0]) != "ccc-three" {
		t.Errorf("unmounted %s, want the least recently used ccc-three", (*unmounted)[0])
	}
	// Unmounting must NOT delete the image: bringing the mount back is one
	// syscall, refetching the image is a NAR download and a mkfs.erofs run.
	if got := imagesOnDisk(t, s); len(got) != 3 {
		t.Errorf("images = %v, want all three kept - unmounting is not deleting", got)
	}
}

// An index describes a tree that no longer exists once its mount is gone.
// Dropping it with the mount is what keeps memory bounded by the same limits as
// the disk and the kernel.
func TestEvictDropsTheIndexWithTheMount(t *testing.T) {
	s, _ := evictStore(t)
	point := filepath.Join(s.mountRoot, "aaa-one")
	if err := os.MkdirAll(point, 0o755); err != nil {
		t.Fatal(err)
	}
	s.mounted[point] = mountFileBacked
	s.idx[filepath.Join(point, "src")+"\x00"+filepath.Join(point, "overlay")] = &srcindex.Index{}
	s.idx["/elsewhere\x00/other"] = &srcindex.Index{}

	if err := s.unmountPoint(point); err != nil {
		t.Fatalf("unmountPoint: %v", err)
	}
	if len(s.idx) != 1 {
		t.Errorf("indexes = %d, want only the unrelated one left", len(s.idx))
	}
	if _, ok := s.idx["/elsewhere\x00/other"]; !ok {
		t.Error("an unrelated index was dropped")
	}
}

// A zero or negative budget disables eviction rather than emptying the volume:
// the alternative reading of "no space allowed" would delete every image on the
// first tick after a typo in the config.
func TestEvictLoopRefusesAZeroBudget(t *testing.T) {
	s, _ := evictStore(t)
	writeImage(t, s, "aaa.erofs", 1<<20, time.Now())
	s.EvictLoop(t.Context(), time.Millisecond, 0, 1) // returns immediately
	if got := imagesOnDisk(t, s); len(got) != 1 {
		t.Errorf("images = %v, want the image kept with eviction disabled", got)
	}
}

// The point of writing the use through to mtime: eviction must still know what
// was in demand after a restart.
//
// The in-memory map is empty on every start, and this service is redeployed far
// more often than a 20 GiB budget fills, so "no map" is the normal case rather
// than the exception. Before this, the fallback was the mtime buildOnce left
// behind - the moment the image was downloaded, never updated - so an image
// fetched months ago and served every day since would be evicted before one
// fetched yesterday and never touched.
func TestLastUseSurvivesARestart(t *testing.T) {
	s, _ := evictStore(t)
	old := time.Now().Add(-90 * 24 * time.Hour)

	// Both downloaded long ago, so mtime alone cannot tell them apart.
	busy := writeImage(t, s, "busy.erofs", 400, time.Time{})
	idle := writeImage(t, s, "idle.erofs", 400, time.Time{})
	for _, p := range []string{busy, idle} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	// One of them gets served.
	s.touch(busy)

	info, err := os.Stat(busy)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(old.Add(time.Hour)) {
		t.Fatalf("touch did not write the use through to the image: mtime is still %v", info.ModTime())
	}

	// Restart: a brand new store over the same directory, with no memory at all.
	fresh, _ := evictStore(t)
	fresh.imageDir = s.imageDir

	if err := fresh.evictOnce(500, 100); err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	got := imagesOnDisk(t, fresh)
	if len(got) != 1 || got[0] != "busy.erofs" {
		t.Errorf("images = %v, want the one that was actually being served", got)
	}
}

// Writing mtime on every request would put a syscall on the hot path for no
// benefit: eviction orders by hours, not milliseconds.
func TestTouchIsThrottled(t *testing.T) {
	s, _ := evictStore(t)
	img := writeImage(t, s, "aaa.erofs", 10, time.Time{})

	s.touch(img)
	first, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}

	// Move the file's mtime backwards. A second touch inside the interval must
	// not put it back, which is how we can tell no write happened.
	marker := time.Now().Add(-42 * time.Hour)
	if err := os.Chtimes(img, marker, marker); err != nil {
		t.Fatal(err)
	}
	s.touch(img)

	after, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(marker) {
		t.Errorf("mtime rewritten inside the throttle window: %v (first write %v)",
			after.ModTime(), first.ModTime())
	}
}
