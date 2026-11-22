package fingerprint

import "testing"

func TestCrossTrackKeepsEarliest(t *testing.T) {
	window := 4
	shared := samples(0, 8) // 2 windows
	v1 := append(append(samples(100, 8), shared...), samples(200, 8)...)
	v2 := append(samples(300, 8), shared...)     // repeats v1's middle
	v3 := append(samples(500, 4), shared[:4]...) // repeats one window too

	h1, _ := NewHasher(window)
	h1.Feed(v1)
	h2, _ := NewHasher(window)
	h2.Feed(v2)
	h3, _ := NewHasher(window)
	h3.Feed(v3)

	dups := FindCrossDuplicates([]TrackView{
		{Name: "v1", Hashes: h1.Hashes(), Samples: v1, Window: window},
		{Name: "v2", Hashes: h2.Hashes(), Samples: v2, Window: window},
		{Name: "v3", Hashes: h3.Hashes(), Samples: v3, Window: window},
	})
	if len(dups) != 3 {
		t.Fatalf("got %d duplicates, want 3: %+v", len(dups), dups)
	}
	for _, d := range dups {
		if d.Earliest != "v1" {
			t.Fatalf("earliest source must be v1: %+v", d)
		}
	}
}

func TestCrossTrackSilentExcluded(t *testing.T) {
	window := 4
	silent := make([]int16, 8)
	h1, _ := NewHasher(window)
	h1.Feed(silent)
	h2, _ := NewHasher(window)
	h2.Feed(silent)
	allSilent := func(int) bool { return true }
	dups := FindCrossDuplicates([]TrackView{
		{Name: "a1", Hashes: h1.Hashes(), Samples: silent, Window: window, Silent: allSilent},
		{Name: "a2", Hashes: h2.Hashes(), Samples: silent, Window: window, Silent: allSilent},
	})
	if len(dups) != 0 {
		t.Fatalf("silence must not produce duplicates: %+v", dups)
	}
}
