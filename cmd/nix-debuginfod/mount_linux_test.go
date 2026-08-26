//go:build linux

package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// These need CAP_SYS_ADMIN and mkfs.erofs, so they skip unless both are there:
//
//	docker run --rm --privileged -v "$PWD":/w -w /w golang:1.26-alpine sh -c \
//	  'apk add --no-cache erofs-utils >/dev/null && go test ./cmd/nix-debuginfod -v'
func requireMountable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs not in $PATH")
	}
	if os.Geteuid() != 0 {
		t.Skip("mounting needs CAP_SYS_ADMIN")
	}
}

// buildImage packs dir into an erofs image and returns its path.
func buildImage(t *testing.T, dir string) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "img.erofs")
	out, err := exec.Command("mkfs.erofs", "--quiet", "-zlz4hc", img, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs.erofs: %s: %v", out, err)
	}
	return img
}

func mustMount(t *testing.T, img, target string) mountKind {
	t.Helper()
	kind, err := mountErofs(img, target)
	if err != nil {
		t.Fatalf("mount %s: %v", img, err)
	}
	t.Cleanup(func() { unmountErofs(target) })
	t.Logf("mounted %s at %s (%s)", filepath.Base(img), target, kind)
	return kind
}

func TestMountErofsRoundTrip(t *testing.T) {
	requireMountable(t)

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "lib", "file"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "mnt")
	mustMount(t, buildImage(t, src), target)

	got, err := os.ReadFile(filepath.Join(target, "lib", "file"))
	if err != nil {
		t.Fatalf("read through the mount: %v", err)
	}
	if string(got) != "content" {
		t.Errorf("content = %q, want %q", got, "content")
	}
}

// The load-bearing property of the whole design: a symlink out of one store path
// into another resolves in the kernel once the target is mounted at its
// canonical location, with no rewriting of the path being opened. Until then the
// open fails with ENOENT and readlink names exactly what is missing - which is
// the signal the lazy fetch loop runs on.
func TestSymlinkAcrossImagesResolvesOnceTargetIsMounted(t *testing.T) {
	requireMountable(t)

	store := t.TempDir() // stands in for /nix/store
	const (
		hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-app"
		hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-libfoo"
	)
	targetB := filepath.Join(store, hashB)

	// B holds the real file.
	srcB := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcB, "lib", "debug"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcB, "lib", "debug", "real.debug"), []byte("REAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A holds only an absolute symlink into B, the way nix store paths refer to
	// each other.
	srcA := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcA, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(targetB, "lib", "debug", "real.debug")
	if err := os.Symlink(linkTarget, filepath.Join(srcA, "lib", "link.debug")); err != nil {
		t.Fatal(err)
	}

	imgA, imgB := buildImage(t, srcA), buildImage(t, srcB)
	mustMount(t, imgA, filepath.Join(store, hashA))

	// The one path the caller ever names. It does not change below.
	path := filepath.Join(store, hashA, "lib", "link.debug")

	_, err := os.ReadFile(path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("before mounting B: err = %v, want ErrNotExist - that is the signal to fetch", err)
	}

	// ENOENT does not say what is missing; readlink does.
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != linkTarget {
		t.Fatalf("readlink = %q, want %q", got, linkTarget)
	}

	mustMount(t, imgB, targetB)

	// Same path, unmodified, now resolves - the kernel crossed the mount.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("after mounting B: %v", err)
	}
	if string(content) != "REAL" {
		t.Errorf("content = %q, want REAL", content)
	}
}

// A mount being read must come away without EBUSY, or eviction would fail
// exactly when the cache is busiest.
func TestUnmountWhileFileIsOpen(t *testing.T) {
	requireMountable(t)

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "mnt")
	mustMount(t, buildImage(t, src), target)

	f, err := os.Open(filepath.Join(target, "file"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := unmountErofs(target); err != nil {
		t.Fatalf("unmount with an open file: %v", err)
	}
	if _, err := f.Read(make([]byte, 1)); err != nil {
		t.Errorf("read after lazy unmount: %v; the fd should stay valid", err)
	}
}
