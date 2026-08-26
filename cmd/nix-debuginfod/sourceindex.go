package main

import (
	"io/fs"
	"path/filepath"
	"slices"
)

// sourceIndex answers "which file did the compiler mean" across BOTH source
// trees at once, by matching paths from the RIGHT.
//
// Matching from the left cannot work. The compiler records a sandbox path like
// /build/glibc-2.42/elf/x.c, or after a ".." in DW_AT_name just /sysdeps/x.c,
// while the tree holds glibc-2.42/sysdeps/x.c - a tarball unpacks into a
// directory of its own, a directory-typed .source output does not, and the
// overlay adds its own level. The number of components to drop from the request
// and the number to add on the tree side are both unknown and differ per
// package, so trying suffixes of the request against a root only works when the
// two happen to cancel out.
//
// From the right there is nothing to guess: index every file by basename, then
// pick the one sharing the most trailing components with the request. This is
// what rev_file_path in the deleted nix_source_files table was for, done against
// the mounted trees instead of a database.
//
// Both trees go into one index rather than being searched in turn, because
// searching them in turn gets the answer wrong: .sourceoverlay would return a
// weak match - a bare basename hit in some unrelated directory - and never give
// .source the chance to offer a far better one. Scored together, the longest
// match wins wherever it lives, and priority only decides ties.
//
// It also removes a class of mistake: the answer is a path produced by walking a
// tree, never the client's string joined onto a root, so a request cannot name a
// file outside the trees no matter what it contains.
type sourceIndex struct {
	byBase map[string][]indexEntry
}

type indexEntry struct {
	root  string   // the tree this file came from
	parts []string // components relative to that root, reversed
	prio  int      // lower wins a tie; trees are given in priority order
}

// newSourceIndex indexes the given trees, highest priority first. A tree that
// cannot be read is skipped rather than failing the lot: a package with no
// overlay must still be able to serve its pristine sources.
func newSourceIndex(roots ...string) (*sourceIndex, error) {
	ix := &sourceIndex{byBase: map[string][]indexEntry{}}
	for prio, root := range roots {
		// WalkDir lstats its root, so given a symlink - which both
		// .sourceoverlay and .source are - it sees a non-directory and walks
		// nothing at all. It has to be resolved first.
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			// A tree that is not there at all - most often a build ID whose
			// archive carries no .sourceoverlay. Skipping is right, but skipping
			// SILENTLY is not: a later 404 then cannot be told apart from "the
			// overlay was searched and did not have it", and that difference is
			// the whole diagnosis when a generated file is missing.
			log.WithError(err).WithField("tree", root).
				Debug("source tree unavailable; it will not be searched")
			continue
		}
		_ = filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a tree we cannot fully read still answers for the rest
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(resolved, p)
			if err != nil {
				return nil
			}
			parts := splitPath(rel)
			slices.Reverse(parts)
			ix.byBase[d.Name()] = append(ix.byBase[d.Name()],
				indexEntry{root: root, parts: parts, prio: prio})
			return nil
		})
	}
	return ix, nil
}

// find returns the full path of the best match for reqPath, or "".
func (ix *sourceIndex) find(reqPath string) string {
	want := splitPath(reqPath)
	if len(want) == 0 {
		return ""
	}
	slices.Reverse(want)

	var best *indexEntry
	bestScore := 0
	for i, cand := range ix.byBase[want[0]] {
		score := commonPrefixLen(cand.parts, want) // both reversed: a prefix here is a suffix there
		switch {
		case score > bestScore:
		case score < bestScore, best == nil:
			if best != nil {
				continue
			}
		// Equal scores: the overlay wins, because it holds what the build
		// actually compiled - patches applied, configure output, generated
		// headers - and the pristine copy would not match the binary. Failing
		// that, the shallower file, so an undistinguished request still resolves
		// the same way every time.
		case cand.prio != best.prio:
			if cand.prio > best.prio {
				continue
			}
		case len(cand.parts) >= len(best.parts):
			continue
		}
		best, bestScore = &ix.byBase[want[0]][i], score
	}
	if best == nil {
		return ""
	}
	fwd := slices.Clone(best.parts)
	slices.Reverse(fwd)
	return filepath.Join(append([]string{best.root}, fwd...)...)
}

func commonPrefixLen(a, b []string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// indexFor builds and caches the index for a build ID's two trees. They are
// read-only erofs images, so it can never go stale; it is built once and kept.
// TODO: evict alongside the mounts it describes.
func (s *store) indexFor(overlay, source string) (*sourceIndex, error) {
	key := overlay + "\x00" + source

	s.idxMu.Lock()
	if ix, ok := s.idx[key]; ok {
		s.idxMu.Unlock()
		return ix, nil
	}
	s.idxMu.Unlock()

	v, err, _ := s.group.Do("index:"+key, func() (any, error) {
		ix, err := newSourceIndex(overlay, source)
		if err != nil {
			return nil, err
		}
		s.idxMu.Lock()
		s.idx[key] = ix
		s.idxMu.Unlock()
		return ix, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*sourceIndex), nil
}

// lookup finds reqPath in either tree, or returns "".
func (s *store) lookup(overlay, source, reqPath string) string {
	ix, err := s.indexFor(overlay, source)
	if err != nil {
		return ""
	}
	return ix.find(reqPath)
}

// searchedTrees names the trees that were actually indexed, so a miss says
// whether the overlay was among them.
func (s *store) searchedTrees(roots ...string) []string {
	var out []string
	for _, root := range roots {
		if _, err := filepath.EvalSymlinks(root); err == nil {
			out = append(out, filepath.Base(root))
		}
	}
	return out
}
