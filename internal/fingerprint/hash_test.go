package fingerprint

import (
	"testing"
)

func samples(seed int16, n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = seed + int16(i)
	}
	return s
}

func TestFindDuplicatesKeepsEarliest(t *testing.T) {
	window := 4
	ref := samples(0, 16)                            // 4 windows
	dup := append(samples(0, 8), samples(100, 8)...) // first 2 windows copied
	rh, _ := NewHasher(window)
	rh.Feed(ref)
	dh, _ := NewHasher(window)
	dh.Feed(dup)
	dups := FindDuplicates("v1", "v2", rh.Hashes(), dh.Hashes(), ref, dup, window, nil)
	if len(dups) != 2 {
		t.Fatalf("got %d duplicates, want 2", len(dups))
	}
	for _, d := range dups {
		if d.Earliest != "v1" || d.RepeatedBy != "v2" {
			t.Fatalf("earliest source not kept: %+v", d)
		}
	}
}

func TestHashCollisionDoubleChecked(t *testing.T) {
	// Force every window to the same hash: all matches become collision
	// candidates and only byte-identical windows may survive.
	orig := hashWindow
	hashWindow = func([]int16) uint64 { return 0xC011 }
	defer func() { hashWindow = orig }()

	window := 4
	ref := samples(0, 8)
	dup := append(samples(0, 4), samples(500, 4)...) // window 0 identical, window 1 differs
	rh, _ := NewHasher(window)
	rh.Feed(ref)
	dh, _ := NewHasher(window)
	dh.Feed(dup)
	dups := FindDuplicates("v1", "v2", rh.Hashes(), dh.Hashes(), ref, dup, window, nil)
	if len(dups) != 1 || dups[0].WindowIndex != 0 {
		t.Fatalf("collision must be rejected by re-verification: %+v", dups)
	}
}

func TestSilentWindowsExcluded(t *testing.T) {
	window := 4
	silent := make([]int16, 8) // all zero in both tracks
	rh, _ := NewHasher(window)
	rh.Feed(silent)
	dh, _ := NewHasher(window)
	dh.Feed(silent)
	dups := FindDuplicates("a1", "a2", rh.Hashes(), dh.Hashes(), silent, silent, window,
		func(int) bool { return true })
	if len(dups) != 0 {
		t.Fatalf("silence must not count as duplicate: %+v", dups)
	}
}

func TestStreamingHasherMatchesBatch(t *testing.T) {
	data := samples(7, 10)
	h, _ := NewHasher(4)
	h.Feed(data[:3])
	h.Feed(data[3:])
	got := h.Hashes()
	if len(got) != 2 { // tail of 2 samples stays unhashed
		t.Fatalf("got %d hashes, want 2", len(got))
	}
	if got[0] != hashWindow(data[0:4]) || got[1] != hashWindow(data[4:8]) {
		t.Fatal("streaming hashes differ from batch hashes")
	}
}

func TestNewHasherRejectsBadWindow(t *testing.T) {
	if _, err := NewHasher(0); err == nil {
		t.Fatal("zero window should fail")
	}
}
