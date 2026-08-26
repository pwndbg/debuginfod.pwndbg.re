package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"
)

// maxResolveHops bounds the lazy resolution loop. Each pass can uncover a
// different missing store path, so this counts iterations rather than link
// depth - and it doubles as the cycle guard, since a symlink loop would
// otherwise spin here forever.
const maxResolveHops = 16

// maxResolveSteps bounds one walk inside missingFor. A path has few components,
// but each symlink splices its target in front of the remainder, so a cycle would
// otherwise loop here rather than in Open.
const maxResolveSteps = 256

// narFetcher is the half of the work that talks to cache.nixos.org and runs
// mkfs.erofs. It is an interface so the store can be tested against prepared
// images, with no network and no mkfs - *nix.NixDebuginfo satisfies it.
type narFetcher interface {
	FetchNarByStorePath(ctx context.Context, storePath, outFile string) error
	// DebuginfoMeta is the cheap half - a 159-byte JSON lookup per build ID.
	DebuginfoMeta(ctx context.Context, buildID string) (archive, member string, err error)
	// FetchNarByArchive is the expensive half, and it is keyed by the archive
	// rather than the build ID because one archive serves many of them.
	FetchNarByArchive(ctx context.Context, archive, outFile string) error
}

// store turns build IDs into bytes.
//
// Each nix store path becomes one erofs image, mounted at its canonical
// <mountRoot>/<hash>-<name>. Mounting at the real path is the whole trick: nix
// store paths refer to each other through absolute symlinks, so once a target is
// mounted where it claims to be, the kernel walks into it by itself. Nothing here
// rewrites a path or reimplements symlink resolution.
//
// What is left for us is the one thing the kernel cannot do - mount something
// that has not been fetched yet. That is what Open's loop is for.
type store struct {
	fetcher   narFetcher
	imageDir  string // <hash>.erofs files
	mountRoot string // canonical mounts: <mountRoot>/<hash>-<name>
	entryRoot string // entry images; nothing ever links into one, so no canonical name

	// group collapses concurrent Ensure calls for the same store path. A cold
	// path is a NAR download plus a mkfs.erofs run - tens of seconds and up to a
	// gigabyte - so two requests needing the same one must not both do it.
	group singleflight.Group

	// slots caps how many *distinct* paths build at once. singleflight only
	// removes duplicate work; without this, twenty different cold build IDs would
	// run twenty simultaneous downloads and mkfs.erofs processes. This is the one
	// job the deleted worker pool did that singleflight does not - a semaphore
	// rather than a queue, because the thing worth bounding is concurrency, and a
	// queue would also have to answer what to do when it fills.
	slots chan struct{}

	mu      sync.Mutex
	mounted map[string]mountKind // mount point -> how it was mounted

	idxMu sync.Mutex
	idx   map[string]*sourceIndex // mounted tree -> its file index
}

func newStore(fetcher narFetcher, imageDir, mountRoot, entryRoot string, maxFetches int) *store {
	if maxFetches < 1 {
		maxFetches = 1
	}
	// Leftovers from a previous process first. Everything under these roots is a
	// mount point we made, and mounts do not survive the process: a container
	// restart builds a new mount namespace, and `docker rm -f` is a SIGKILL that
	// runs nothing deferred. What does survive is the empty DIRECTORY each mount
	// point left behind.
	//
	// That is not cosmetic. The whole lazy path keys on ENOENT: os.Open fails,
	// missingFor says which store path is absent, Ensure mounts it. An empty
	// leftover directory makes os.Open *succeed*, so nothing is ever fetched or
	// mounted, the index walks an empty tree and every source request 404s -
	// but only after a restart, and only until the cache directory is wiped.
	//
	// Removed with os.Remove, never RemoveAll: it deletes an empty directory and
	// refuses anything else, so a surprise here is left alone rather than
	// destroyed.
	reconcileMountRoots(mountRoot, entryRoot)

	// The mount root has to exist before anything is looked up under it.
	// missingFor walks a path component by component and stops at the first one
	// that does not resolve; with /nix absent it gave up on "/nix" - which is not
	// a store path and never will be - and so never reached the component it
	// could have fetched. A request naming a store path directly then 404'd
	// without a single fetch attempt, while requests arriving through .source
	// worked, because those start inside an already-mounted image.
	//
	// mountErofs creates the ancestors too, but only once a mount happens, and
	// the first mount is the thing being blocked.
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		log.WithError(err).WithField("path", mountRoot).Error("cannot create the mount root")
	}
	return &store{
		fetcher:   fetcher,
		imageDir:  imageDir,
		mountRoot: mountRoot,
		entryRoot: entryRoot,
		mounted:   map[string]mountKind{},
		idx:       map[string]*sourceIndex{},
		slots:     make(chan struct{}, maxFetches),
	}
}

// acquire blocks until a build slot is free. It is taken around the build only,
// never around a mount or across a call that itself builds, so it cannot
// deadlock on itself.
func (s *store) acquire(ctx context.Context) (release func(), err error) {
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// reconcileMountRoots clears mount points left behind by a previous process.
// See the call site in newStore for why an empty directory is worse than a
// missing one.
func reconcileMountRoots(roots ...string) {
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // not there yet: nothing to reconcile
		}
		for _, e := range entries {
			point := filepath.Join(root, e.Name())
			// Unmount first in case one somehow outlived us; ignore the error,
			// because "was not mounted" is the expected case.
			_ = unmountErofs(point)
			if err := os.Remove(point); err != nil {
				log.WithError(err).WithField("path", point).
					Warn("stale mount point left in place; it was not an empty directory")
			}
		}
	}
}

func (s *store) isMounted(point string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.mounted[point]
	return ok
}

// nixBase32 is the alphabet nix hashes use - note the missing e, o, u and t.
const nixBase32 = "0123456789abcdfghijklmnpqrsvwxyz"

// isStoreHash checks the shape of a store path hash: exactly 32 characters of
// nix base32.
//
// This is not pedantry. A source request can name a store path directly, so this
// runs on a string the client chose, and each accepted one costs a narinfo fetch
// and possibly a NAR download. Rejecting junk here keeps garbage from turning
// into upstream traffic.
func isStoreHash(h string) bool {
	if len(h) != 32 {
		return false
	}
	for _, c := range h {
		if !strings.ContainsRune(nixBase32, c) {
			return false
		}
	}
	return true
}

// storePathOf returns the <mountRoot>/<hash>-<name> prefix of target, or "" if
// target does not name a store path - a relative link, one pointing somewhere
// else entirely, or something merely shaped like a store path.
// splitStorePath splits target into the store path it names and the remainder
// inside it, or "", "" if it does not name one.
//
// The hash is lowercased on the way through: GCC sometimes emits it uppercase in
// the ELF, while the store path on disk - and so our mount point - is always
// lowercase, and a request carrying one would otherwise fail validation and then
// miss the mount.
//
// This is the right place for that, and nix.NixStorePathToHash is not: that
// works on path.Base, so for a path pointing *inside* a store path it sees the
// filename, finds no "-", and calls log.Fatalf - taking the process down. Here
// the store path component has already been isolated and validated.
func (s *store) splitStorePath(target string) (storePath, rest string) {
	tail, ok := strings.CutPrefix(target, s.mountRoot+string(os.PathSeparator))
	if !ok {
		return "", ""
	}
	name, rest, _ := strings.Cut(tail, string(os.PathSeparator))
	hash, pkg, found := strings.Cut(name, "-")
	if !found {
		return "", ""
	}
	hash = strings.ToLower(hash)
	if !isStoreHash(hash) {
		return "", ""
	}
	return filepath.Join(s.mountRoot, hash+"-"+pkg), rest
}

func (s *store) storePathOf(target string) string {
	sp, _ := s.splitStorePath(target)
	return sp
}

// missingFor answers the question ENOENT does not: which store path is absent.
//
// It resolves path one component at a time, following symlinks itself, and stops
// at the first one whose target names a store path that is not mounted. Doing the
// walk manually is only a diagnostic - once the missing path is mounted, the
// kernel does the real resolution.
//
// Following *through* links that already resolve is the part that matters: a
// chain A -> B -> C is discovered one link per pass, and after B is mounted the
// second pass has to walk past it to notice that C is missing. An earlier version
// stopped as soon as the first link pointed somewhere already mounted, which made
// any chain longer than one hop fail. It also has to carry the unresolved tail
// across a link, or a dangling link below an already-resolved directory is
// invisible.
func (s *store) missingFor(path string) string {
	parts := splitPath(path)
	cur := string(os.PathSeparator)

	for steps := 0; steps < maxResolveSteps; steps++ {
		if len(parts) == 0 {
			return "" // fully resolved: nothing here is missing
		}
		next := filepath.Join(cur, parts[0])

		fi, err := os.Lstat(next)
		if err != nil {
			// A path straight into a store path nobody has mounted yet - no
			// symlink led us here, the caller just asked for it.
			if sp := s.storePathOf(next); sp != "" && !s.isMounted(sp) {
				return sp
			}
			return ""
		}

		if fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(next)
			if err != nil {
				return ""
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(cur, target)
			}
			if sp := s.storePathOf(target); sp != "" && !s.isMounted(sp) {
				return sp
			}
			// Reachable, or not ours. Splice the target in front of whatever is
			// left and keep going.
			parts = append(splitPath(target), parts[1:]...)
			cur = string(os.PathSeparator)
			continue
		}

		cur = next
		parts = parts[1:]
	}
	return ""
}

// splitPath cleans p and returns its components. Shared by missingFor here and
// by sourceIndex in sourceindex.go. It does NOT drop "..": on an
// absolute path filepath.Clean has already resolved every one, and on a relative
// path the caller has to decide what a leading ".." means. sourceCandidates
// drops them; GetSource cleans its input before anything else looks at it.
//
// SplitSeq rather than Split: the intermediate slice is never used for anything
// but this loop.
// splitPath cleans p and returns its components. Shared by missingFor here and
// by sourceIndex in sourceindex.go. It deliberately does NOT drop
// "..": on an absolute path filepath.Clean has already resolved every one and
// cannot climb above the root, and on a relative path it is the caller who
// decides what a leading ".." means. sourceCandidates is where they get dropped,
// because that is the one place a client-supplied path gets joined onto a root.
func splitPath(p string) []string {
	var out []string
	for part := range strings.SplitSeq(filepath.Clean(p), string(os.PathSeparator)) {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
}

// Open resolves path, fetching and mounting whatever it turns out to need. It
// also reports how many store paths had to be pulled in on the way, which is
// worth knowing: for some endpoints the answer should always be zero.
func (s *store) Open(ctx context.Context, path string) (*os.File, int, error) {
	for hop := 0; hop < maxResolveHops; hop++ {
		f, err := os.Open(path)
		if err == nil {
			return f, hop, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, hop, err
		}

		missing := s.missingFor(path)
		if missing == "" {
			return nil, hop, err
		}
		if err := s.Ensure(ctx, missing); err != nil {
			return nil, hop, fmt.Errorf("resolving %s: %w", missing, err)
		}
	}
	return nil, maxResolveHops, fmt.Errorf("%s: gave up after %d hops; symlink cycle?", path, maxResolveHops)
}

// Ensure makes storePath readable at its canonical location.
func (s *store) Ensure(ctx context.Context, storePath string) error {
	_, err, _ := s.group.Do(storePath, func() (any, error) {
		if s.isMounted(storePath) {
			return nil, nil
		}
		name := filepath.Base(storePath)
		hash, _, _ := strings.Cut(name, "-")

		image := filepath.Join(s.imageDir, hash+".erofs")
		if err := s.buildOnce(ctx, image, func(tmp string) error {
			return s.fetcher.FetchNarByStorePath(ctx, storePath, tmp)
		}); err != nil {
			return nil, err
		}
		return nil, s.mount(image, storePath)
	})
	return err
}

// buildOnce produces image if it is not already there, via a temp file and a
// rename. Without the rename a half-written image left by a crash or a killed
// container would be indistinguishable from a complete one on the next start.
func (s *store) buildOnce(ctx context.Context, image string, build func(tmp string) error) error {
	if _, err := os.Stat(image); err == nil {
		return nil
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	// Re-checked after waiting: something else may have built it while this call
	// sat in the queue.
	if _, err := os.Stat(image); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(image), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(image), filepath.Base(image)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	// mkfs.erofs writes the file itself; it only needs the name.
	os.Remove(tmpName)
	defer os.Remove(tmpName)

	if err := build(tmpName); err != nil {
		return err
	}
	return os.Rename(tmpName, image)
}

func (s *store) mount(image, point string) error {
	kind, err := mountErofs(image, point)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.mounted[point] = kind
	s.mu.Unlock()
	log.WithField("image", filepath.Base(image)).
		WithField("at", point).WithField("kind", string(kind)).Debug("mounted")
	return nil
}

// GetDebuginfo serves the debug info for buildID.
//
// The entry image is mounted under entryRoot rather than at a canonical store
// path, because the debuginfo endpoint hands back a bare nar/<filehash>.nar.xz
// with no store-path identity - and it needs none: links point *out* of this
// image, never into it.
//
// Debug info is always self-contained in that one NAR: the member is a real file
// inside it, e.g. lib/debug/.build-id/8a/e0b6....debug. So this endpoint should
// never need a second store path. It still goes through Open, which costs nothing
// when the first os.Open succeeds, but a non-zero hop count means that assumption
// has broken somewhere in nixpkgs - and silently fetching another NAR would hide
// it. Hence the warning rather than either a hard failure or nothing at all.
//
// The lazy resolution Open implements is for /source/* and /executable, where the
// file genuinely lives in a different store path - sources in the source output,
// the binary in the main one. Neither endpoint exists yet.
func (s *store) GetDebuginfo(ctx context.Context, buildID string, w http.ResponseWriter) error {
	point, member, archive, err := s.entry(ctx, buildID, w)
	if err != nil {
		return err
	}
	f, hops, err := s.Open(ctx, filepath.Join(point, member))
	if err != nil {
		return err
	}
	defer f.Close()
	if hops > 0 {
		log.WithField("buildid", buildID).WithField("member", member).WithField("hops", hops).
			Warn("debuginfo needed another store path; it is supposed to be self-contained")
	}

	setMeta(w, f.Name(), executableTarget(point, member), archive)
	_, err = io.Copy(w, f)
	return err
}

// archiveKeyRe bounds what may become a filename. The archive path comes from
// cache.nixos.org, so it is not ours; a nar file hash is 52 characters of nix
// base32, and anything else is refused rather than joined onto a path.
var archiveKeyRe = regexp.MustCompile(`^[0-9a-z]{20,60}$`)

// keyFromImageName reads a key back out of "archive-<key>.erofs".
//
// It exists because the two directions are not symmetric: archiveKey parses a
// binary cache path, while the symlink points at an image FILE. Feeding the
// image name to archiveKey silently failed every time - "archive-<key>" is not a
// bare key - so the link never read back and every request re-fetched the
// metadata it was supposed to remember.
func keyFromImageName(name string) (string, bool) {
	key, ok := strings.CutPrefix(name, "archive-")
	if !ok {
		return "", false
	}
	key, ok = strings.CutSuffix(key, ".erofs")
	if !ok || !archiveKeyRe.MatchString(key) {
		return "", false
	}
	return key, true
}

// archiveNameFile holds an archive's path in the binary cache, written beside
// its image so a warm request can report x-debuginfod-archive without asking the
// network for something it already downloaded.
func (s *store) archiveNameFile(key string) string {
	return filepath.Join(s.imageDir, "archive-"+key+".name")
}

// archiveKey turns "nar/1rnrd....nar.xz" into "1rnrd...".
func archiveKey(archive string) (string, error) {
	name := filepath.Base(archive)
	key, _, _ := strings.Cut(name, ".")
	if !archiveKeyRe.MatchString(key) {
		return "", fmt.Errorf("unusable archive name %q", archive)
	}
	return key, nil
}

// entry mounts the -debug image for a build ID and returns its mount point plus
// the member path inside it.
//
// Keyed by the ARCHIVE, not the build ID. /debuginfo/<buildid> hands back an
// archive that many build IDs share - one glibc NAR covers 389 of them - so
// keying by build ID downloaded and repacked the same 11.8 MB once per build ID
// and kept 389 identical images. The per-build-ID part is only the JSON lookup,
// which is cached in a sidecar so a warm request touches the network not at all.
func (s *store) entry(ctx context.Context, buildID string, w http.ResponseWriter) (point, member, archive string, err error) {
	if len(buildID) < 3 {
		return "", "", "", fmt.Errorf("build id %q too short", buildID)
	}
	key, archive, err := s.archiveFor(ctx, buildID)
	if err != nil {
		return "", "", "", err
	}

	// The build ID resolves, so the answer is going to be 200 unless something
	// downstream breaks - which is exactly what a 103 is for. Sent here rather
	// than as a 200, because a 200 would commit the status and forfeit answering
	// 404 or 500 if the NAR download or mkfs.erofs then fails.
	//
	// Everything after this point can take tens of seconds on a cold archive,
	// and until now the client has heard nothing at all.
	//
	// Synchronous, on this goroutine: the middleware's timer needs a mutex
	// because Timer.Stop does not wait for a callback already running, but there
	// is no such race here.
	sendEarlyHints(w, archive)

	point = filepath.Join(s.entryRoot, key)
	image := filepath.Join(s.imageDir, "archive-"+key+".erofs")

	_, err, _ = s.group.Do("archive:"+key, func() (any, error) {
		if err := s.buildOnce(ctx, image, func(tmp string) error {
			// The archive path is never reconstructed from the key. Its
			// extension is not ours to guess - the binary cache already moved
			// narinfo NARs from .nar.xz to .nar.zst - so it comes from the
			// lookup. buildOnce only runs this when the image is actually
			// missing, so a JSON fetch here sits next to an 11.8 MB download and
			// costs nothing.
			if archive == "" {
				a, _, err := s.fetcher.DebuginfoMeta(ctx, buildID)
				if err != nil {
					return err
				}
				archive = a
			}
			if err := s.fetcher.FetchNarByArchive(ctx, archive, tmp); err != nil {
				return err
			}
			// Written before buildOnce renames the image into place, so "image
			// exists" implies "its name is recorded" - the same ordering the
			// member sidecar used to need.
			return os.WriteFile(s.archiveNameFile(key), []byte(archive), 0o644)
		}); err != nil {
			return nil, err
		}
		if s.isMounted(point) {
			return nil, nil
		}
		return nil, s.mount(image, point)
	})
	if err != nil {
		return "", "", "", err
	}
	member, err = memberIn(point, buildID)
	return point, member, archive, err
}

// memberIn finds a build ID's debug file inside a mounted archive.
//
// The path is not stored anywhere, because the .build-id layout puts it at
// <first two hex>/<rest> - a function of the build ID. But it is *probed* rather
// than assumed, because there are two spellings: older NARs name the file with
// no suffix, newer ones append ".debug". Which one a given archive uses depends
// on its age, so the archive is asked instead of guessed.
//
// Probing also means the member is known to exist before anything is served,
// where computing it would have deferred the failure to an ENOENT deeper down.
func memberIn(point, buildID string) (string, error) {
	base := filepath.Join("lib", "debug", ".build-id", buildID[:2], buildID[2:])
	for _, cand := range []string{base + ".debug", base} {
		// Lstat, not Stat: the member is normally a regular file, but if it is a
		// symlink into another store path, following it here would fail on a
		// target that has not been mounted yet - and mounting it is precisely
		// what Open does afterwards. Accept the link and let resolution happen
		// where it belongs.
		fi, err := os.Lstat(filepath.Join(point, cand))
		if err != nil {
			continue
		}
		if fi.Mode().IsRegular() || fi.Mode()&fs.ModeSymlink != 0 {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%s not in %s: %w", buildID, filepath.Base(point), fs.ErrNotExist)
}

// archiveFor answers which archive holds a build ID, without asking the network
// twice for something that cannot change.
//
// The answer is recorded as a SYMLINK from the build ID to the shared image,
// which is what the relation actually is: hundreds of build IDs pointing at one
// archive. A text sidecar would say the same thing less honestly - the link is
// visible in ls -l, needs no parsing, and when eviction deletes an archive the
// build IDs that used it are left as dangling links, which is exactly how you
// find them.
// The returned archive path is empty when the answer came from the link: the
// caller only needs it if it has to rebuild the image, and asks then.
func (s *store) archiveFor(ctx context.Context, buildID string) (key, archive string, err error) {
	link := filepath.Join(s.imageDir, "buildid", buildID)
	if target, err := os.Readlink(link); err == nil {
		if k, ok := keyFromImageName(filepath.Base(target)); ok {
			// The archive's full name is kept once per ARCHIVE, not per build
			// ID, so this does not reintroduce the 389-copies problem. It cannot
			// be rebuilt from the key: the extension is not ours to guess, since
			// the binary cache already moved narinfo NARs to .nar.zst.
			raw, err := os.ReadFile(s.archiveNameFile(k))
			if err != nil {
				return k, "", nil
			}
			return k, string(raw), nil
		}
	}

	archive, _, err = s.fetcher.DebuginfoMeta(ctx, buildID)
	if err != nil {
		return "", "", err
	}
	key, err = archiveKey(archive)
	if err != nil {
		return "", "", err
	}

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return "", "", err
	}
	// Written via a temp name and renamed: a half-created link would otherwise
	// be indistinguishable from a good one. Best effort - losing it costs one
	// extra lookup, nothing more.
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink("../archive-"+key+".erofs", tmp); err == nil {
		if err := os.Rename(tmp, link); err != nil {
			log.WithError(err).WithField("buildid", buildID).Warn("could not record the archive link")
		}
	}
	return key, archive, nil
}

// GetSource serves a source file for buildID.
//
// Two places hold sources, and the order between them is not arbitrary:
//
//	<id>.sourceoverlay -> ../../../../src/overlay   patched and generated files
//	<id>.source        -> /nix/store/<hash>-...     the pristine upstream source
//
// The overlay wins. It carries what the build actually compiled - patches
// applied, configure output, generated headers - so serving the pristine file
// where an overlay exists would hand back source that does not match the binary.
//
// .source is either an archive or a directory, and this code does not care
// which: nix.NarUnpackerAsTarball branches on the NAR root type, so a regular
// file is unpacked through detectCompression and a directory is copied straight
// across. Either way what gets mounted is a tree.
func (s *store) GetSource(ctx context.Context, buildID, reqPath string, w http.ResponseWriter) error {
	// A compiler sometimes records an absolute store path instead of a sandbox
	// one - headers from a dependency, or a file compiled straight out of the
	// store. That file is in neither the overlay nor .source, so it needs its own
	// NAR.
	//
	// This branch comes first and does no suffix matching, because the request is
	// an exact reference rather than a guess. Falling through to the heuristic
	// would be actively harmful: asking for
	// /nix/store/<hash>-other-pkg/include/stdio.h could match an unrelated
	// include/stdio.h in this package's overlay and serve the wrong file with no
	// sign anything went wrong. Open already handles it - missingFor treats a
	// path leading into an unmounted store path as something to fetch.
	reqPath = filepath.Clean(reqPath)

	if sp, rest := s.splitStorePath(reqPath); sp != "" {
		f, _, err := s.Open(ctx, filepath.Join(sp, rest))
		if err != nil {
			return err
		}
		defer f.Close()
		setMeta(w, f.Name(), reqPath, sp)
		_, err = io.Copy(w, f)
		return err
	}

	// reqPath is client-controlled and is about to be opened under, or joined
	// onto, a mounted filesystem. Cleaning it first is what contains a traversal:
	// on an absolute path Clean resolves every ".." lexically and cannot climb
	// above the root, so "/nix/store/<hash>-x/../../etc/passwd" collapses to
	// "/etc/passwd" - which then stops looking like a store path, and the branch
	// below declines it.
	//
	// Without this the store-path branch opened the raw string directly, and the
	// kernel resolved ".." *through the mount*, walking out of the store and
	// reading host files. sourceCandidates was never the hole; this branch
	// bypasses it entirely.
	point, member, archive, err := s.entry(ctx, buildID, w)
	if err != nil {
		return err
	}

	base := filepath.Join(point, strings.TrimSuffix(member, ".debug"))
	overlay := base + ".sourceoverlay"
	source := base + ".source"

	// The source tree is not local yet, and the index needs both trees, so it
	// gets mounted even when the overlay turns out to hold the answer. That is
	// the price of scoring them together - and a small one, since the overlay
	// only carries patched and generated files, so most requests need .source
	// anyway. This Open also says whether the build ID has sources at all.
	dir, _, err := s.Open(ctx, source)
	if err != nil {
		return err
	}
	dir.Close()

	// Logged on the miss path only: a source file that is generated during the
	// build - CMake output under a build/ directory, say - is simply not in the
	// source tree, and the only place it could be is the overlay. Saying which
	// trees were actually searched turns "404" into something diagnosable.
	if hit := s.lookup(overlay, source, reqPath); hit != "" {
		file, from := describeSourceHit(hit, overlay, source, archive)
		setMeta(w, hit, file, from)
		return streamFile(hit, w)
	}
	log.WithField("buildid", buildID).WithField("path", reqPath).
		WithField("trees", s.searchedTrees(overlay, source)).
		Info("source not found in any tree")
	return fmt.Errorf("%s: %w", reqPath, fs.ErrNotExist)
}

// sendEarlyHints announces "this exists, the answer will be 200" without
// committing to it.
//
// x-status is not a real debuginfod header; it says what the final status is
// expected to be, which is the one thing worth knowing while the build runs. It
// is removed afterwards so it does not also appear on the response that carries
// the actual status - a header claiming 200 next to a 404 would be worse than no
// header at all.
func sendEarlyHints(w http.ResponseWriter, archive string) {
	w.Header().Set("x-status", strconv.Itoa(http.StatusOK))
	if archive != "" {
		w.Header().Set("x-debuginfod-archive", archive)
	}
	w.WriteHeader(http.StatusEarlyHints)
	w.Header().Del("x-status")
}

// describeSourceHit says where a source file came from, which is not one answer
// but two: the overlay lives inside the -debug NAR, the pristine tree in a store
// path of its own. Naming the NAR for a file that came from the tarball would be
// wrong, and vice versa.
//
// The store path is read with Readlink rather than taken from the opened
// directory. Opening <id>.source yields a *File whose Name is the path as
// opened - the symlink itself - and the symlink is an internal detail; what
// identifies the source is its target, /nix/store/<hash>-glibc-2.42.tar.xz.
//
// file is reported relative to whichever tree answered. The absolute path would
// expose our mount points, which mean nothing outside this process - and for the
// overlay it would be doubly useless, since that tree has no name anyone else
// knows.
func describeSourceHit(hit, overlay, source, archive string) (file, from string) {
	if rel, err := filepath.Rel(overlay, hit); err == nil && !strings.HasPrefix(rel, "..") {
		return rel, archive
	}
	if rel, err := filepath.Rel(source, hit); err == nil && !strings.HasPrefix(rel, "..") {
		target, err := os.Readlink(source)
		if err != nil {
			// The tree is mounted - we just read from it - so this should not
			// happen; report the file without claiming where it came from
			// rather than naming something wrong.
			return rel, ""
		}
		return rel, target
	}
	return hit, archive
}

// setMeta fills the debuginfod response headers, and must be called before any
// of the body goes out: once the status is committed the header block is closed
// and only trailers could follow.
//
//	x-debuginfod-size     the payload's size, so a client can show progress
//	x-debuginfod-file     which file this is; for debug info that is the binary
//	                      it belongs to, read off the .executable sibling
//	x-debuginfod-archive  the NAR it was extracted from
//
// Size is taken from the file about to be served rather than from anything
// remembered, so it cannot drift from what the client actually receives.
func setMeta(w http.ResponseWriter, path, file, archive string) {
	if fi, err := os.Stat(path); err == nil {
		w.Header().Set("x-debuginfod-size", strconv.FormatInt(fi.Size(), 10))
	}
	if file != "" {
		w.Header().Set("x-debuginfod-file", file)
	}
	if archive != "" {
		w.Header().Set("x-debuginfod-archive", archive)
	}
}

// executableTarget reads the .executable sibling to learn which binary a build
// ID's debug info describes, e.g.
// /nix/store/<hash>-glibc-2.42-61/lib/gconv/IBM1142.so. Empty when the archive
// is old enough not to carry the sibling links.
func executableTarget(point, member string) string {
	target, err := os.Readlink(filepath.Join(point, strings.TrimSuffix(member, ".debug")+".executable"))
	if err != nil {
		return ""
	}
	return target
}

func streamFile(path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// GetExecutable serves the unstripped binary for buildID.
//
// The -debug NAR carries, next to each <id>.debug file, three sibling symlinks
// naming everything else that build ID has:
//
//	lib/debug/.build-id/8a/e0b6...debug           the debug info (a real file)
//	lib/debug/.build-id/8a/e0b6...executable  ->  /nix/store/<hash>-glibc-2.42-61/lib/gconv/IBM1142.so
//	lib/debug/.build-id/8a/e0b6...source      ->  /nix/store/<hash>-glibc-2.42.tar.xz
//	lib/debug/.build-id/8a/e0b6...sourceoverlay -> ../../../../src/overlay
//
// So nothing has to be indexed, derived from DWARF, or looked up in narinfo
// References: the answer is a symlink, and following it is exactly what Open
// does. This is the first endpoint that actually needs the lazy resolution -
// debuginfo never leaves its own NAR.
func (s *store) GetExecutable(ctx context.Context, buildID string, w http.ResponseWriter) error {
	point, member, archive, err := s.entry(ctx, buildID, w)
	if err != nil {
		return err
	}

	// The siblings are the member with its extension swapped.
	link := filepath.Join(point, strings.TrimSuffix(member, ".debug")+".executable")

	f, hops, err := s.Open(ctx, link)
	if err != nil {
		return err
	}
	defer f.Close()
	log.WithField("buildid", buildID).WithField("hops", hops).Debug("serving executable")

	setMeta(w, f.Name(), executableTarget(point, member), archive)
	_, err = io.Copy(w, f)
	return err
}
