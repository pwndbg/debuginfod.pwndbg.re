package main

import "testing"

func TestClassifyAssetRealNames(t *testing.T) {
	for _, tc := range []struct {
		name              string
		variant, plat, fm string
	}{
		{"pwndbg_2026.02.18_x86_64-portable.tar.xz", "pwndbg-gdb", "linux/x86_64", ".tar.xz"},
		{"pwndbg_2026.02.18_arm64-portable.tar.xz", "pwndbg-gdb", "linux/arm64", ".tar.xz"},
		{"pwndbg_2026.02.18_macos_arm64-portable.tar.xz", "pwndbg-gdb", "macos/arm64", ".tar.xz"},
		{"pwndbg-lldb_2026.02.18_macos_arm64-portable.tar.xz", "pwndbg-lldb", "macos/arm64", ".tar.xz"},
		{"pwndbg_2026.02.18_amd64.deb", "pwndbg-gdb", "linux/x86_64", ".deb"},
		{"pwndbg-lldb_2026.02.18_amd64.deb", "pwndbg-lldb", "linux/x86_64", ".deb"},
	} {
		got := classifyAsset(tc.name)
		if got.Variant != tc.variant || got.Platform != tc.plat || got.Format != tc.fm {
			t.Errorf("%s -> %+v, want {%s %s %s}", tc.name, got, tc.variant, tc.plat, tc.fm)
		}
	}
}

// A ".tar.xz" must not be read as an unknown format because a shorter suffix
// matched first, and an unparseable name must land in "other" rather than
// inventing a bucket from whatever the string happened to contain.
func TestClassifyAssetFallsBackToOther(t *testing.T) {
	for _, name := range []string{"", "garbage", "a_b_c.weird"} {
		got := classifyAsset(name)
		if got.Platform != unknownBucket {
			t.Errorf("%q platform = %q, want %q", name, got.Platform, unknownBucket)
		}
	}
	if got := classifyAsset("pwndbg_2026.02.18_riscv64.deb"); got.Platform != unknownBucket {
		t.Errorf("unknown arch -> %q, want %q", got.Platform, unknownBucket)
	}
}

// topN must not silently drop the tail: the bars are shown with percentages that
// have to add up to the total printed above them.
func TestTopNFoldsTailIntoOther(t *testing.T) {
	in := []labelCount{{"a", 10}, {"b", 9}, {"c", 8}, {"d", 7}, {"e", 6}}
	var want uint64
	for _, it := range in {
		want += it.N
	}

	got := topN(in, 2)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (2 + other)", len(got))
	}
	var sum uint64
	for _, it := range got {
		sum += it.N
	}
	if sum != want {
		t.Errorf("total = %d, want %d", sum, want)
	}
	if got[2].Label != unknownBucket || got[2].N != 21 {
		t.Errorf("tail = %+v, want {%s 21}", got[2], unknownBucket)
	}
}

// An "other" bucket already produced by classifyAsset must absorb the tail rather
// than appear twice.
func TestTopNMergesExistingOther(t *testing.T) {
	got := topN([]labelCount{{"a", 10}, {unknownBucket, 5}, {"c", 3}, {"d", 1}}, 2)
	seen := 0
	for _, it := range got {
		if it.Label == unknownBucket {
			seen++
			if it.N != 9 {
				t.Errorf("other = %d, want 9", it.N)
			}
		}
	}
	if seen != 1 {
		t.Errorf("%d 'other' rows, want 1", seen)
	}
}

// The variant bucket is derived from the request path, so it must be a closed
// vocabulary: anything outside it becomes a bar labelled with a string a client
// chose. Escaping keeps that safe, but an unbounded set of bars is unreadable
// regardless - and this is the property that keeps it bounded.
func TestVariantVocabularyIsClosed(t *testing.T) {
	allowed := map[string]bool{"pwndbg-gdb": true, "pwndbg-lldb": true, unknownBucket: true}
	for _, name := range []string{
		"pwndbg_1.0_amd64.deb",
		"pwndbg-gdb_1.0_amd64.deb",
		"pwndbg-lldb_1.0_amd64.deb",
		"evil_1.0_amd64.deb",
		"<script>_1.0_amd64.deb",
		"_1.0_amd64.deb",
		"PWNDBG_1.0_amd64.deb", // case is not folded - it is not our asset name
		"",
	} {
		if got := classifyAsset(name).Variant; !allowed[got] {
			t.Errorf("classifyAsset(%q).Variant = %q, outside the allowed set", name, got)
		}
	}
	// The two we ship still land in their own buckets rather than all collapsing.
	if got := classifyAsset("pwndbg_2026.02.18_amd64.deb").Variant; got != "pwndbg-gdb" {
		t.Errorf("gdb build -> %q, want pwndbg-gdb", got)
	}
	if got := classifyAsset("pwndbg-lldb_2026.02.18_amd64.deb").Variant; got != "pwndbg-lldb" {
		t.Errorf("lldb build -> %q, want pwndbg-lldb", got)
	}
}
