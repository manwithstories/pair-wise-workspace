package clock

import (
	"testing"

	"review/internal/issues"
)

func TestUnwrapRTPWrap(t *testing.T) {
	base := uint32(0xFFFFFFF0)
	raw := []uint32{base, base + 100, base + 200, base + 300} // wraps at 2nd packet
	recs, iss := UnwrapRTP(raw, 100)
	if len(iss) != 0 {
		t.Fatalf("issues: %v", iss)
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Unwrapped != recs[i-1].Unwrapped+100 {
			t.Fatalf("not monotonic at %d: %d -> %d", i, recs[i-1].Unwrapped, recs[i].Unwrapped)
		}
	}
	if !recs[1].Wrapped {
		t.Fatal("wrap not flagged")
	}
	if recs[3].Unwrapped != uint64(base)+300 {
		t.Fatalf("bad expansion: %d", recs[3].Unwrapped)
	}
}

func TestUnwrapRTPAnomalies(t *testing.T) {
	raw := []uint32{1000, 1100, 1100, 1200, 1150, 1300, 1600}
	//             ok    ok    dup   ok    back  ok    gap(300)
	recs, iss := UnwrapRTP(raw, 100)
	kinds := map[string]int{}
	for _, is := range iss {
		kinds[is.Kind]++
	}
	if kinds[issues.RTPDuplicate] != 1 || kinds[issues.RTPBackward] != 1 || kinds[issues.RTPGap] != 1 {
		t.Fatalf("kinds: %v", kinds)
	}
	// The backward glitch must not poison later packets.
	if recs[5].Unwrapped != 1300 || recs[6].Unwrapped != 1600 {
		t.Fatalf("bad series: %+v", recs)
	}
}

func TestUnwrapRTPEmpty(t *testing.T) {
	recs, iss := UnwrapRTP(nil, 100)
	if len(recs) != 0 || len(iss) != 0 {
		t.Fatal("empty input must be quiet")
	}
}
