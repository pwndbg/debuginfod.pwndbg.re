package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tree is one unpacked source package on the volume.
type tree struct {
	Key      string    `json:"key"` // component/source_version
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Files    int       `json:"files"`
	Modified time.Time `json:"modified"`
}

// handleTrees lists what is on the volume.
//
// The unpacker reports, it does not decide. Which trees to keep is a question
// about request traffic, and this service sees none of it - cmd/deb-debuginfod
// does, so it owns the policy and calls back here to act on it. Splitting it
// the other way would give two services separate opinions about what is cached,
// which is how they start disagreeing.
func (s *server) handleTrees(w http.ResponseWriter, _ *http.Request) {
	out := []tree{}
	components, err := os.ReadDir(s.root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, c := range components {
		if !c.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.root, c.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			// Skip staging directories: a .tmp-* is an unpack in progress, and
			// reporting it would invite the evictor to delete it mid-write.
			if !e.IsDir() || len(e.Name()) == 0 || e.Name()[0] == '.' {
				continue
			}
			path := filepath.Join(s.root, c.Name(), e.Name())
			if _, err := os.Stat(filepath.Join(path, doneMarker)); err != nil {
				continue // not finished, not ours to report
			}
			files, bytes := measure(path)
			mod := time.Time{}
			if info, err := e.Info(); err == nil {
				mod = info.ModTime()
			}
			out = append(out, tree{
				Key: c.Name() + "/" + e.Name(), Path: path,
				Bytes: bytes, Files: files, Modified: mod,
			})
		}
	}
	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleDelete removes one tree.
//
// The rename comes first so the tree stops being visible the instant this is
// called, even though RemoveAll on twenty thousand files takes a moment. Doing
// it the other way round leaves a window in which the directory exists, has its
// done-marker, and is missing half its files - which reads as a complete tree
// to anything that looks.
//
// A reader holding the tree open is not a problem: unlink on Linux keeps the
// data alive until the last descriptor closes, so an in-flight response
// finishes on the bytes it already opened.
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("component") + "/" + r.PathValue("name")
	if !safeKey(r.PathValue("component"), r.PathValue("name")) {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.root, r.PathValue("component"), r.PathValue("name"))
	if _, err := os.Stat(path); err != nil {
		w.WriteHeader(http.StatusNoContent) // already gone; deleting twice is not an error
		return
	}

	doomed := path + ".deleting"
	if err := os.Rename(path, doomed); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.RemoveAll(doomed); err != nil {
		// The rename already succeeded, so the tree is unreachable and the
		// space will be reclaimed by the next sweep. Report it rather than
		// failing the eviction that has, in the way that matters, worked.
		log.WithError(err).WithField("tree", key).Warn("tree unlinked but not fully removed")
	}
	log.WithField("tree", key).Info("evicted")
	w.WriteHeader(http.StatusNoContent)
}

// safeKey re-checks the path components. They arrive from another service, but
// they still become a filesystem path, and "it is internal" is not a property
// the filesystem can verify.
func safeKey(component, name string) bool {
	if !reComp.MatchString(component) {
		return false
	}
	return name != "" && name == filepath.Base(name) && name[0] != '.'
}

// sweepStale removes staging directories abandoned by a killed process.
//
// Only past staleAfter: a younger .tmp-* may belong to an unpack running right
// now, and deleting that would fail a live request to reclaim nothing. Same
// reasoning as the abandoned .tmp-* sweep in cmd/proxy's file cache.
func (s *server) sweepStale(staleAfter time.Duration) {
	components, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	for _, c := range components {
		dir := filepath.Join(s.root, c.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() {
				continue
			}
			// A package-named directory with no done-marker cannot be produced
			// by this service: the marker is written into the staging directory
			// BEFORE the atomic rename, so a final name always implies a
			// complete tree. One appearing here therefore came from somewhere
			// else - a restored backup, an older build, a hand-placed tree.
			//
			// Reported and left alone rather than deleted. It is invisible to
			// /trees, so eviction will never reclaim it and it would otherwise
			// sit there for ever unnoticed; but deleting something an operator
			// put here deliberately is the worse mistake of the two.
			if name[0] != '.' && !strings.HasSuffix(name, ".deleting") {
				if _, err := os.Stat(filepath.Join(dir, name, doneMarker)); err != nil {
					log.WithField("path", filepath.Join(dir, name)).
						Warn("directory has no completion marker: invisible to /trees and never evicted")
				}
				continue
			}
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < staleAfter {
				continue
			}
			p := filepath.Join(dir, name)
			if err := os.RemoveAll(p); err != nil {
				log.WithError(err).WithField("path", p).Warn("could not reclaim abandoned directory")
				continue
			}
			log.WithField("path", p).Info("reclaimed abandoned unpack")
		}
	}
}
