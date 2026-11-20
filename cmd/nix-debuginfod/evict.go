package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Eviction for the erofs images and the mounts that go with them.
//
// Until this existed nothing here was ever removed: images accumulated on the
// volume, mounts accumulated in the kernel, and source indexes accumulated in
// memory, for as long as the process ran. That is not merely untidy - a
// /source/* request may name ANY store path, so what gets downloaded and
// mounted is steered by the client, not by our own traffic.
//
// Three limits are in play and disk is only the first: each mount costs kernel
// memory and counts against fs.mount-max, and each mounted tree keeps a source
// index of a few megabytes.
//
// What is deliberately NOT deleted: the archive-<key>.name sidecars and the
// buildid/<id> symlinks. They are a few hundred bytes each and they are the
// memory of "which NAR this key came from" - keeping them means a re-request
// rebuilds the image without another metadata round trip to the binary cache.
// Eviction should free the expensive bytes, not the cheap knowledge.

type evictable struct {
	image string // <imageDir>/<hash>.erofs or <imageDir>/archive-<key>.erofs
	point string // its mount point, empty when it is not mounted
	bytes int64
	used  time.Time
}

// touchPersistInterval is how often a use is written through to the filesystem.
// Hourly is far finer than eviction needs and keeps this off the hot path: the
// in-memory map answers every request, and only the first use in an hour costs
// a syscall.
const touchPersistInterval = time.Hour

// touch records that an image was just used, in memory and on the image itself.
//
// The mtime write is what makes eviction survive a restart. Without it the
// in-memory map is the only record of demand, and it is empty after every
// redeploy - so ordering fell back to the file's mtime, which was the moment
// buildOnce created it and never changed afterwards. That is "oldest download",
// not "least recently used", and since this service is redeployed far more often
// than a 10 GiB budget fills, that fallback was the normal case rather than the
// exception: a package fetched in August and served every day since would be
// evicted before one fetched yesterday and never touched again.
//
// So mtime here deliberately means **last used**, not "when built". Nothing else
// reads it - buildOnce only tests the file for existence - and the two now agree
// instead of quietly meaning different things.
func (s *store) touch(image string) {
	now := time.Now()

	s.usedMu.Lock()
	if s.used == nil {
		s.used = map[string]time.Time{}
	}
	last, seen := s.used[image]
	if seen && now.Sub(last) < touchPersistInterval {
		s.usedMu.Unlock()
		return
	}
	s.used[image] = now
	s.usedMu.Unlock()

	if err := os.Chtimes(image, now, now); err != nil && !os.IsNotExist(err) {
		// Not fatal: losing this costs eviction ordering after the next
		// restart, not correctness now.
		log.WithError(err).WithField("image", image).Debug("could not record last use on the image")
	}
}

// touchPath marks the image behind a path that was just served. The path lies
// under a mount point, and both mount layouts put the point exactly one level
// below their root, so the point is recoverable from the path alone.
func (s *store) touchPath(path string) {
	for _, root := range []string{s.mountRoot, s.entryRoot} {
		rest, ok := strings.CutPrefix(path, root+string(os.PathSeparator))
		if !ok {
			continue
		}
		base, _, _ := strings.Cut(rest, string(os.PathSeparator))
		if base != "" {
			s.touch(s.imageForPoint(filepath.Join(root, base)))
		}
		return
	}
}

// imageForPoint derives an image path from a mount point. The two layouts are
// fixed by mount() and entry(): a store path is mounted at
// <mountRoot>/<hash>-<name> and built from <imageDir>/<hash>.erofs, an entry at
// <entryRoot>/<key> from <imageDir>/archive-<key>.erofs.
func (s *store) imageForPoint(point string) string {
	base := filepath.Base(point)
	if filepath.Dir(point) == s.entryRoot {
		return filepath.Join(s.imageDir, "archive-"+base+".erofs")
	}
	hash, _, _ := strings.Cut(base, "-")
	return filepath.Join(s.imageDir, hash+".erofs")
}

// EvictLoop keeps the image directory under budget.
//
// Rebuilding here is expensive in a way it is not for cmd/deb-debuginfod - a
// NAR download plus a mkfs.erofs run, tens of seconds to minutes - so the
// budget should be generous and the sweep infrequent. Evicting something that
// is about to be asked for again costs a great deal more than the disk it
// freed.
func (s *store) EvictLoop(ctx context.Context, interval time.Duration, budget int64, maxMounts int) {
	if budget <= 0 {
		log.Warn("image cache budget is zero or negative; eviction disabled")
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.evictOnce(budget, maxMounts); err != nil && ctx.Err() == nil {
			// Never fatal. Failing to evict means the volume grows, which is a
			// problem later; stopping the loop makes it permanent.
			log.WithError(err).Warn("eviction pass failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// evictOnce enforces two separate limits, in this order.
//
// The order matters and so does the asymmetry between them. Unmounting is
// almost free to undo - the image is still on disk, so coming back costs one
// mount syscall - while deleting an image costs a NAR download and a mkfs.erofs
// run. So the mount limit can be enforced aggressively, and the byte budget
// should not be: over-evicting mounts is cheap, over-evicting images is not.
//
// Mounts are their own resource, not a proxy for disk. Each one occupies kernel
// memory (~700 kB measured) and counts against fs.mount-max, so a process can
// be well under its byte budget and still be the reason nothing on the host can
// mount anything.
func (s *store) evictOnce(budget int64, maxMounts int) error {
	s.evictMounts(maxMounts)

	entries, err := os.ReadDir(s.imageDir)
	if err != nil {
		return err
	}

	// Which images are currently mounted, so eviction can unmount before
	// deleting rather than leaving a mount pointing at a file that is gone.
	s.mu.Lock()
	pointOf := make(map[string]string, len(s.mounted))
	for point := range s.mounted {
		pointOf[s.imageForPoint(point)] = point
	}
	s.mu.Unlock()

	s.usedMu.Lock()
	used := make(map[string]time.Time, len(s.used))
	for k, v := range s.used {
		used[k] = v
	}
	s.usedMu.Unlock()

	var images []evictable
	var total int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".erofs") {
			continue // buildid/ and the .name sidecars are kept, see the note above
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		image := filepath.Join(s.imageDir, name)
		when, ok := used[image]
		if !ok {
			// Not served since this process started, so the answer comes from
			// the filesystem. touch keeps mtime meaning "last used", so this is
			// the same measure as the map rather than a weaker stand-in.
			when = info.ModTime()
		}
		images = append(images, evictable{
			image: image, point: pointOf[image], bytes: info.Size(), used: when,
		})
		total += info.Size()
	}

	if total <= budget {
		log.WithField("bytes", total).WithField("budget", budget).
			WithField("images", len(images)).Debug("image cache within budget")
		return nil
	}

	// Oldest use first, with the name as a tie-break so two images with the
	// same timestamp are not ordered by directory iteration - an eviction pass
	// that picks differently every run cannot be reasoned about from the logs.
	sort.Slice(images, func(i, j int) bool {
		if !images[i].used.Equal(images[j].used) {
			return images[i].used.Before(images[j].used)
		}
		return images[i].image < images[j].image
	})

	freed := int64(0)
	for _, img := range images {
		if total-freed <= budget {
			break
		}
		if err := s.evictImage(img); err != nil {
			log.WithError(err).WithField("image", img.image).Warn("could not evict")
			continue
		}
		freed += img.bytes
	}
	log.WithField("was", total).WithField("freed", freed).
		WithField("budget", budget).Info("image cache evicted")
	return nil
}

// evictMounts unmounts the least recently used mounts until at most maxMounts
// remain. The images stay on disk: this reclaims kernel resources, not bytes,
// and the next request for one of these paths mounts it again without any
// network work.
func (s *store) evictMounts(maxMounts int) {
	if maxMounts <= 0 {
		return
	}
	s.mu.Lock()
	points := make([]string, 0, len(s.mounted))
	for point := range s.mounted {
		points = append(points, point)
	}
	s.mu.Unlock()

	if len(points) <= maxMounts {
		return
	}

	s.usedMu.Lock()
	used := make(map[string]time.Time, len(s.used))
	for k, v := range s.used {
		used[k] = v
	}
	s.usedMu.Unlock()

	sort.Slice(points, func(i, j int) bool {
		ti, tj := used[s.imageForPoint(points[i])], used[s.imageForPoint(points[j])]
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return points[i] < points[j]
	})

	dropped := 0
	for _, point := range points[:len(points)-maxMounts] {
		if err := s.unmountPoint(point); err != nil {
			log.WithError(err).WithField("point", point).Warn("could not unmount")
			continue
		}
		dropped++
	}
	log.WithField("was", len(points)).WithField("unmounted", dropped).
		WithField("max", maxMounts).Info("mounts trimmed")
}

// unmountPoint detaches one mount and forgets everything derived from it. The
// image is left alone.
//
// MNT_DETACH means a mount still being read does not fail with EBUSY - the
// kernel keeps it alive until the last open descriptor closes - which matters
// because this runs precisely when the service is busiest.
func (s *store) unmountPoint(point string) error {
	if err := s.unmount(point); err != nil {
		return err
	}
	// os.Remove, never RemoveAll: the mount point is an empty directory now, and
	// anything else under it is not ours to delete.
	if err := os.Remove(point); err != nil && !os.IsNotExist(err) {
		log.WithError(err).WithField("point", point).
			Debug("mount point left behind; reconcileMountRoots will clear it")
	}
	s.mu.Lock()
	delete(s.mounted, point)
	s.mu.Unlock()

	// The index describes a tree that is no longer reachable. Dropping it here
	// is what keeps memory bounded by the same limits as the disk and the
	// kernel.
	s.dropIndexesUnder(point)
	return nil
}

// evictImage unmounts and deletes one image.
//
// The order is unmount, then delete the image: the other way round leaves a
// live mount backed by a file that no longer exists. MNT_DETACH means a mount
// still being read does not fail with EBUSY - the kernel keeps it alive until
// the last open descriptor closes - which matters because eviction runs
// precisely when the service is busiest.
func (s *store) evictImage(img evictable) error {
	if img.point != "" {
		if err := s.unmountPoint(img.point); err != nil {
			return err
		}
	}

	if err := os.Remove(img.image); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.usedMu.Lock()
	delete(s.used, img.image)
	s.usedMu.Unlock()

	log.WithField("image", filepath.Base(img.image)).
		WithField("bytes", img.bytes).Info("evicted")
	return nil
}

// dropIndexesUnder forgets every source index built from a tree under point.
// The keys are "<overlay>\x00<source>", both paths inside a mounted image, so a
// prefix match on either half is what identifies them.
func (s *store) dropIndexesUnder(point string) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	for key := range s.idx {
		overlay, source, _ := strings.Cut(key, "\x00")
		if strings.HasPrefix(overlay, point) || strings.HasPrefix(source, point) {
			delete(s.idx, key)
		}
	}
}
