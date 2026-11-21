package index

import "testing"

func TestKeyframeIndexSkipsBadAndNonKey(t *testing.T) {
	k := NewKeyframeIndexer()
	views := []FrameView{
		{ID: 0, SSRC: 1, PTSUs: 0, Keyframe: true, FirstTick: 0},
		{ID: 1, SSRC: 1, PTSUs: 33000, FirstTick: 1},              // not a keyframe
		{ID: 2, SSRC: 1, PTSUs: 66000, Keyframe: true, Bad: true}, // bad keyframe
		{ID: 3, SSRC: 1, PTSUs: 99000, Keyframe: true, FirstTick: 3},
	}
	for _, v := range views {
		k.Add(v)
	}
	got := k.Result().Entries
	if len(got) != 2 || got[0].FrameID != 0 || got[1].FrameID != 3 {
		t.Fatalf("index entries: %+v", got)
	}
	if got[0].SHA256 == "" || got[0].SHA256 == got[1].SHA256 {
		t.Fatal("entries must carry distinct SHA-256 evidence")
	}
}

func TestAddIdempotent(t *testing.T) {
	k := NewKeyframeIndexer()
	v := FrameView{ID: 5, SSRC: 1, PTSUs: 100, Keyframe: true}
	k.Add(v)
	k.Add(v) // resume replay must not create a duplicate entry
	if len(k.Result().Entries) != 1 {
		t.Fatalf("idempotent add produced %d entries", len(k.Result().Entries))
	}
}

func TestLookupAtOrBefore(t *testing.T) {
	k := NewKeyframeIndexer()
	for _, pts := range []int64{0, 100, 250, 400} {
		k.Add(FrameView{ID: uint32(pts / 50), SSRC: 1, PTSUs: pts, Keyframe: true})
	}
	idx := k.Result()
	if e, ok := idx.Lookup(240); !ok || e.PTSUs != 100 {
		t.Fatalf("lookup 240: %+v %v", e, ok)
	}
	if _, ok := idx.Lookup(-1); ok {
		t.Fatal("lookup before first keyframe must miss")
	}
	if e, ok := idx.Lookup(1000); !ok || e.PTSUs != 400 {
		t.Fatalf("lookup past end: %+v", e)
	}
}

func TestHitRate(t *testing.T) {
	k := NewKeyframeIndexer()
	k.Add(FrameView{ID: 0, Keyframe: true})
	k.Add(FrameView{ID: 2, Keyframe: true})
	// ID 4 is a bad truth keyframe and is correctly absent.
	idx := k.Result()
	truth := []uint32{0, 2, 4, 6}
	bad := map[uint32]bool{4: true}
	rate := idx.HitRate(truth, bad)
	// Total truth excluding bad = 3 (0,2,6); hits 2 -> 2/3.
	if d := abs(rate - 2.0/3.0); d > 1e-9 {
		t.Fatalf("hit rate: got %v want 0.6667", rate)
	}
	// Perfect recovery against indexed truth hits 1.000.
	perfect := idx.HitRate([]uint32{0, 2}, nil)
	if perfect != 1.0 {
		t.Fatalf("perfect hit rate: %v", perfect)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
