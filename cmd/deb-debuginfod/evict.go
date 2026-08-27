package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// tree mirrors what cmd/deb-unpack reports about the volume.
type tree struct {
	Key      string    `json:"key"`
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Files    int       `json:"files"`
	Modified time.Time `json:"modified"`
}

// touch records that a package was just used. Called on every served request,
// which is what makes the eviction order reflect demand rather than the order
// things happened to be unpacked in.
func (s *srv) touch(key string) {
	s.mu.Lock()
	s.lastUsed[key] = time.Now()
	s.mu.Unlock()
}

// EvictLoop keeps the volume under budget.
//
// Least-recently-used by request, not by mtime: mtime says when a tree was
// unpacked, which is the same for a package fetched once and never touched
// again as for one serving a debugger all week. Trees this process has never
// served - after a restart, say - fall back to mtime, which at least orders
// them among themselves.
//
// Rebuilding a tree costs about 1.2 s here (download plus dpkg-source) against
// minutes for an erofs image in cmd/nix-debuginfod, so this can afford to be
// far more aggressive than that service will be able to.
func (s *srv) EvictLoop(ctx context.Context, interval time.Duration, budget int64) {
	if budget <= 0 {
		log.Warn("source cache budget is zero or negative; eviction disabled")
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.evictOnce(ctx, budget); err != nil && ctx.Err() == nil {
			// Never fatal. Failing to evict means the volume grows, which is a
			// problem for later; failing loudly and stopping would make it a
			// problem for ever.
			log.WithError(err).Warn("eviction pass failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *srv) evictOnce(ctx context.Context, budget int64) error {
	trees, err := s.listTrees(ctx)
	if err != nil {
		return err
	}

	var total int64
	for _, t := range trees {
		total += t.Bytes
	}
	if total <= budget {
		log.WithField("bytes", total).WithField("budget", budget).
			WithField("trees", len(trees)).Debug("source cache within budget")
		return nil
	}

	// Oldest use first. The tie-break on Key is not cosmetic: without it the
	// order of two never-served trees with identical mtimes depends on map
	// iteration, and an eviction pass that picks differently every run is
	// impossible to reason about from the logs.
	s.mu.Lock()
	used := make(map[string]time.Time, len(s.lastUsed))
	for k, v := range s.lastUsed {
		used[k] = v
	}
	s.mu.Unlock()

	sort.Slice(trees, func(i, j int) bool {
		ti, oki := used[trees[i].Key]
		tj, okj := used[trees[j].Key]
		if !oki {
			ti = trees[i].Modified
		}
		if !okj {
			tj = trees[j].Modified
		}
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return trees[i].Key < trees[j].Key
	})

	freed := int64(0)
	for _, t := range trees {
		if total-freed <= budget {
			break
		}
		// Drop the index BEFORE asking for the delete. The other order leaves a
		// window in which the index still offers paths whose files are already
		// gone, and every request in it fails with a 502 rather than paying for
		// a re-unpack.
		s.mu.Lock()
		delete(s.indexes, t.Key)
		delete(s.lastUsed, t.Key)
		s.mu.Unlock()

		if err := s.deleteTree(ctx, t.Key); err != nil {
			log.WithError(err).WithField("tree", t.Key).Warn("could not evict")
			continue
		}
		freed += t.Bytes
	}

	log.WithField("was", total).WithField("freed", freed).
		WithField("budget", budget).Info("source cache evicted")
	return nil
}

func (s *srv) listTrees(ctx context.Context) ([]tree, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.unpacker+"/trees", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing trees: %s", resp.Status)
	}
	var out []tree
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (s *srv) deleteTree(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.unpacker+"/tree/"+key, nil)
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deleting %s: %s", key, resp.Status)
	}
	return nil
}
