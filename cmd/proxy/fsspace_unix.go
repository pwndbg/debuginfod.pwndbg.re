//go:build unix

// The package does not build outside unix anyway - filecache.go builds its
// fan-out on syscall.Dup - so this tag documents the constraint rather than
// opening a door to portability. There is no fallback variant, because it would
// have nothing to rescue.

package main

import (
	"os"
	"syscall"
)

// diskBytes returns the space a file actually occupies, not its length.
//
// This matters because CACHE_PATH sits on btrfs: with compression enabled
// st_blocks describes the size AFTER compression, while info.Size() still
// reports the apparent length. The difference also runs the other way - a file
// shorter than a block still occupies a whole one - so a sum of lengths is not
// occupancy in either direction.
//
// st_blocks is always in 512 B units regardless of the filesystem's block size;
// POSIX guarantees this and Linux follows it.
func diskBytes(info os.FileInfo) uint64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return uint64(info.Size())
	}
	return uint64(st.Blocks) * 512
}

// fsSpace returns the capacity and free space of the partition holding path.
//
// Bavail, not Bfree: Bfree also counts the root reserve (5% by default on ext4),
// which the proxy process cannot use anyway. The alarm should fire when space
// runs out for us, not when it runs out for root.
//
// Bsize has a different type per platform - int64 on Linux, uint32 on Darwin -
// hence the explicit conversion instead of multiplying directly.
func fsSpace(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return st.Blocks * bsize, st.Bavail * bsize, nil
}
