package main

import (
	"path/filepath"

	"github.com/pwndbg/debuginfod.pwndbg.re/srcindex"
)

func (s *store) indexFor(overlay, source string) (*srcindex.Index, error) {
	key := overlay + "\x00" + source

	s.idxMu.Lock()
	if ix, ok := s.idx[key]; ok {
		s.idxMu.Unlock()
		return ix, nil
	}
	s.idxMu.Unlock()

	v, err, _ := s.group.Do("index:"+key, func() (any, error) {
		ix, err := srcindex.New(overlay, source)
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
	return v.(*srcindex.Index), nil
}

// lookup finds reqPath in either tree, or returns "".
func (s *store) lookup(overlay, source, reqPath string) string {
	ix, err := s.indexFor(overlay, source)
	if err != nil {
		return ""
	}
	return ix.Find(reqPath)
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
