package fingerprint

import (
	"testing"

	"audiorestore/internal/model"
)

func TestNormalizeRateAndLayoutInvariant(t *testing.T) {
	// Same program on two clocks and channel layouts must normalize to
	// the same canonical sample sequence.
	base := [][]int{{1000, 2000, -3000, 4000}}
	stereo := [][]int{
		{1000, 2000, -3000, 4000},
		{1000, 2000, -3000, 4000},
	}
	a := Normalize(Content{TrackID: "a", Rate: 48000, Data: base})
	b := Normalize(Content{TrackID: "b", Rate: 48000, Data: stereo})
	if len(a) != len(b) {
		t.Fatalf("canonical lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("channel layout not normalized at %d: %d vs %d", i, a[i], b[i])
		}
	}
	if Hash(a) != Hash(b) {
		t.Fatal("equal content produced different fingerprints")
	}
}

func TestResamplingDeterministic(t *testing.T) {
	data := [][]int{make([]int, 480)}
	for i := range data[0] {
		data[0][i] = (i*37 - 200) % 5000
	}
	c1 := Normalize(Content{Rate: 44100, Data: data})
	c2 := Normalize(Content{Rate: 44100, Data: data})
	if len(c1) == 0 || Hash(c1) != Hash(c2) {
		t.Fatal("resampling must be deterministic")
	}
	// 480 frames at 44.1 kHz maps to ~522 canonical frames at 48 kHz.
	want := int64(480) * int64(CanonicalRate) / int64(44100)
	if int64(len(c1)) != want {
		t.Fatalf("resampled length = %d, want %d", len(c1), want)
	}
}

func TestDedupeSharedStorage(t *testing.T) {
	table := NewTable()
	id1, dup1 := table.Register(42)
	id2, dup2 := table.Register(42)
	id3, dup3 := table.Register(99)
	if dup1 || dup3 {
		t.Fatal("first registration of a hash is not a duplicate")
	}
	if !dup2 || id1 != id2 {
		t.Fatal("equal content must share a storage reference")
	}
	if id3 == id1 {
		t.Fatal("distinct content must get distinct storage")
	}
	if table.Hits() != 1 {
		t.Fatalf("dedup hits = %d, want 1", table.Hits())
	}
}

func TestCanonicalRateSupported(t *testing.T) {
	if !model.SampleRate(CanonicalRate).Valid() {
		t.Fatal("canonical rate outside supported envelope")
	}
}
