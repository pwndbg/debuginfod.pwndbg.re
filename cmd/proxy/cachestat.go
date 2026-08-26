package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cacheUsage is one measurement of the cache directory and of the partition it
// lives on. Blobs and temp files are counted separately because they are two
// different things: the first are the goods we want to keep, the second are
// leftovers from interrupted downloads whose growth is a symptom, not capacity.
type cacheUsage struct {
	Timestamp time.Time
	Entries   uint64 // number of blobs
	// Bytes is the space actually allocated on disk (st_blocks); ApparentBytes is
	// the sum of file lengths. On btrfs the two drift apart - compression shrinks
	// the former, block rounding inflates it for small files - and the
	// CACHE_MAX_BYTES budget is about occupancy, not length.
	Bytes         uint64
	ApparentBytes uint64
	TmpBytes      uint64 // unfinished downloads, also as allocated space
	FsTotal       uint64
	FsFree        uint64
}

// scanCacheUsage counts; it deletes nothing. It deliberately does not reuse
// evictOnce: that function has three tests pinning its error behaviour and
// returns early when CACHE_MAX_BYTES <= 0 - which is exactly the configuration
// where usage matters most, since without eviction the directory grows unbounded.
func scanCacheUsage(dir string) (cacheUsage, error) {
	u := cacheUsage{Timestamp: time.Now()}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// An unreadable root makes the whole measurement worthless - a zero
			// written to the database would look like an empty cache. Every other
			// error is ordinary work: download() creates and removes .tmp-* files
			// while the scan is running.
			if path == dir {
				return err
			}
			return nil //nolint:nilerr // one entry vanished or is unreadable
		}
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		switch {
		case strings.Contains(base, ".tmp-"):
			u.TmpBytes += diskBytes(info)
		case strings.HasSuffix(base, ".meta"):
			u.Bytes += diskBytes(info)
			u.ApparentBytes += uint64(info.Size())
		case isBlobName(base):
			u.Bytes += diskBytes(info)
			u.ApparentBytes += uint64(info.Size())
			u.Entries++
		}
		return nil
	})
	if err != nil {
		return cacheUsage{}, err
	}

	total, free, err := fsSpace(dir)
	if err != nil {
		// A missing statfs does not invalidate the directory measurement itself - store what we have.
		log.WithError(err).Warn("cache stats: could not read free space")
	} else {
		u.FsTotal, u.FsFree = total, free
	}
	return u, nil
}

// cacheStatStore is an interface for the same reason as accessLogger and
// stateStore - so the worker tests need no ClickHouse.
type cacheStatStore interface {
	InsertCacheUsage(ctx context.Context, u cacheUsage) error
}

type cacheStatCollector struct {
	dir string
	db  cacheStatStore
}

func NewCacheStatCollector(dir string, db cacheStatStore) *cacheStatCollector {
	return &cacheStatCollector{dir: dir, db: db}
}

func (c *cacheStatCollector) Worker(ctx context.Context, every time.Duration) {
	c.collect(ctx)

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.collect(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (c *cacheStatCollector) collect(ctx context.Context) {
	u, err := scanCacheUsage(c.dir)
	if err != nil {
		log.WithError(err).Error("cache stats: directory scan failed")
		return
	}
	if err := c.db.InsertCacheUsage(ctx, u); err != nil {
		log.WithError(err).Error("cache stats: insert failed")
		return
	}
	log.WithField("entries", u.Entries).
		WithField("bytes", u.Bytes).
		WithField("fs_free", u.FsFree).Debug("cache stats: recorded")
}
