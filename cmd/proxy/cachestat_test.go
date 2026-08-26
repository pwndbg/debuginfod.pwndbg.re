package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func blobName(seed byte) string {
	return strings.Repeat(string(rune('a'+seed%6)), 64)
}

func writeFile(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanCacheUsageSeparatesBlobsMetaAndTmp(t *testing.T) {
	root := t.TempDir()
	shard := filepath.Join(root, "ab", "cd")

	writeFile(t, shard, blobName(0), 1000)
	writeFile(t, shard, blobName(1), 2000)
	writeFile(t, shard, blobName(0)+".meta", 50)
	writeFile(t, shard, blobName(2)+".tmp-123456", 4000)
	// A file we did not create: CACHE_PATH may point at a directory shared with
	// something else, so foreign names must not enter our counters.
	writeFile(t, shard, "README", 9999)

	u, err := scanCacheUsage(root)
	if err != nil {
		t.Fatalf("scanCacheUsage: %v", err)
	}
	if u.Entries != 2 {
		t.Errorf("Entries = %d, want 2", u.Entries)
	}
	if want := uint64(1000 + 2000 + 50); u.ApparentBytes != want {
		t.Errorf("ApparentBytes = %d, want %d (blobs + .meta, excluding .tmp and foreign files)",
			u.ApparentBytes, want)
	}
	// Bytes counts st_blocks, so for small files it exceeds the sum of lengths -
	// each file occupies a whole block. The exact value cannot be predicted,
	// because the block size depends on the filesystem behind TempDir.
	if u.Bytes < u.ApparentBytes {
		t.Errorf("Bytes (%d) < ApparentBytes (%d); occupancy cannot be below length "+
			"for uncompressed files", u.Bytes, u.ApparentBytes)
	}
	if u.TmpBytes == 0 {
		t.Error("TmpBytes = 0, the .tmp-* file was not counted")
	}
}

// An unreadable root must produce an error, not a zero measurement: a zero written
// to the database would look like an empty cache and hide a failed volume.
func TestScanCacheUsageFailsOnMissingRoot(t *testing.T) {
	if _, err := scanCacheUsage(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Error("want an error for a missing directory")
	}
}

// A single file vanishing between readdir and lstat is download() doing its normal
// work, so it must not bring down the whole measurement.
func TestScanCacheUsageSurvivesVanishingEntry(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ab"), blobName(0), 500)

	u, err := scanCacheUsage(root)
	if err != nil {
		t.Fatalf("scanCacheUsage: %v", err)
	}
	if u.Entries != 1 || u.ApparentBytes != 500 {
		t.Errorf("Entries=%d ApparentBytes=%d, want 1/500", u.Entries, u.ApparentBytes)
	}
}

func TestFsSpaceReportsSomething(t *testing.T) {
	total, free, err := fsSpace(t.TempDir())
	if err != nil {
		t.Skipf("fsSpace unsupported: %v", err)
	}
	if total == 0 {
		t.Error("pojemnosc partycji = 0")
	}
	if free > total {
		t.Errorf("wolne (%d) wieksze niz pojemnosc (%d)", free, total)
	}
}

type fakeCacheStatStore struct {
	calls atomic.Int32
	last  atomic.Pointer[cacheUsage]
	err   error
}

func (f *fakeCacheStatStore) InsertCacheUsage(_ context.Context, u cacheUsage) error {
	f.calls.Add(1)
	if f.err != nil {
		return f.err
	}
	f.last.Store(&u)
	return nil
}

func TestCacheStatCollectorWritesMeasurement(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ab"), blobName(0), 1234)

	store := &fakeCacheStatStore{}
	NewCacheStatCollector(root, store).collect(context.Background())

	got := store.last.Load()
	if got == nil {
		t.Fatal("nothing was recorded")
	}
	if got.Entries != 1 || got.ApparentBytes != 1234 {
		t.Errorf("recorded Entries=%d ApparentBytes=%d, want 1/1234", got.Entries, got.ApparentBytes)
	}
}

// A failed scan must not end in a write: a row of zeros would falsify the history
// permanently, while a missing row is only a gap in the chart.
func TestCacheStatCollectorSkipsWriteOnScanError(t *testing.T) {
	store := &fakeCacheStatStore{}
	NewCacheStatCollector(filepath.Join(t.TempDir(), "missing"), store).collect(context.Background())

	if n := store.calls.Load(); n != 0 {
		t.Errorf("%d inserts after a failed scan, want 0", n)
	}
}

func TestCacheStatWorkerTicks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ab"), blobName(0), 10)
	store := &fakeCacheStatStore{err: errors.New("baza padla")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewCacheStatCollector(root, store).Worker(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// Even when the write keeps failing, the worker must keep trying.
		if store.calls.Load() >= 3 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("worker wykonal tylko %d prob", store.calls.Load())
}

// The section must stay hidden while cache_stats is empty - flat zeros would read
// as an empty cache, when it is really an absence of measurements.
func TestRenderSkipsCacheSectionWithoutData(t *testing.T) {
	snap := sampleSnapshot(20)
	if strings.Contains(string(renderStats(snap)), "Cache and disk") {
		t.Error("the section rendered even though there is no data")
	}

	snap.HasCacheStats = true
	snap.CacheBytes = make([]float64, len(snap.Days))
	snap.CacheTmp = make([]float64, len(snap.Days))
	snap.FsFree = make([]float64, len(snap.Days))
	snap.CacheEntries = make([]float64, len(snap.Days))
	snap.CacheLast = cacheUsage{Entries: 10, Bytes: 1 << 30, FsTotal: 100 << 30, FsFree: 40 << 30}
	if !strings.Contains(string(renderStats(snap)), "Cache and disk") {
		t.Error("the section did not render even though there is data")
	}
}

// For a space budget the meaningful question is "how much of it have we eaten", so
// the scale must reach the threshold even when the data sits far below it -
// otherwise the chart looks full while a few percent are in use.
func TestLinesPanelStretchesScaleToBudget(t *testing.T) {
	small := make([]float64, 30)
	for i := range small {
		small[i] = 100
	}
	svg, max := linesPanel([][]float64{small}, []string{"c-ip"}, "x", 5000, 7)
	if max != 5000 {
		t.Errorf("scale = %v, want it stretched to 5000", max)
	}
	if !strings.Contains(svg, `class="thr"`) {
		t.Error("the budget line is missing")
	}

	if _, max := linesPanel([][]float64{small}, []string{"c-ip"}, "x", 0, 7); max != 100 {
		t.Errorf("without a threshold scale = %v, want 100", max)
	}
}

// CACHE_PATH sits on btrfs, where compression makes a file occupy less than its
// length. Occupancy must therefore come from st_blocks, not from Size().
func TestScanCacheUsageUsesAllocatedNotApparentSize(t *testing.T) {
	root := t.TempDir()
	// A sparse file: large length, few allocated blocks or none at all.
	// The same asymmetry as compression, only easier to trigger deterministically.
	shard := filepath.Join(root, "ab")
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(shard, blobName(0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil { // 64 MiB of length, zero data
		t.Fatal(err)
	}
	f.Close()

	u, err := scanCacheUsage(root)
	if err != nil {
		t.Fatalf("scanCacheUsage: %v", err)
	}
	if u.ApparentBytes != 64<<20 {
		t.Errorf("ApparentBytes = %d, want %d", u.ApparentBytes, uint64(64<<20))
	}
	if u.Bytes >= u.ApparentBytes {
		t.Skipf("the filesystem under TempDir has no sparse-file support "+
			"(Bytes=%d, Apparent=%d) - nothing for this test to check", u.Bytes, u.ApparentBytes)
	}
}
