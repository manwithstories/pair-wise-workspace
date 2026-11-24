package timeline

import "testing"

func repWith(n int, shift func(i int) int64) RepBoundaries {
	rb := RepBoundaries{Representation: "r", FrameDur90k: 3000, Eligible: true}
	for i := 0; i < n; i++ {
		pts := int64(i) * 90000
		if shift != nil {
			pts += shift(i)
		}
		rb.Boundaries = append(rb.Boundaries, KeyBoundary{PTS90k: pts, Sequence: uint32(i + 1)})
	}
	return rb
}

func TestAlignEightCommonBoundaries(t *testing.T) {
	hi := repWith(8, nil)
	hi.Representation = "high"
	mid := repWith(8, nil)
	mid.Representation = "mid"
	low := repWith(8, nil)
	low.Representation = "low"
	// exercise rational-ish exact offsets on one rep
	for i := range low.Boundaries {
		low.Boundaries[i].PTS90k = int64(i) * 90000
	}
	rep := Align([]RepBoundaries{low, hi, mid}) // unsorted on purpose
	if rep.Aligned != 8 || rep.NotSwitchable != 0 {
		t.Fatalf("want 8 aligned, 0 failed, got aligned=%d failed=%d", rep.Aligned, rep.NotSwitchable)
	}
	if len(rep.Points[0].Members) != 3 {
		t.Fatalf("want 3 members, got %d", len(rep.Points[0].Members))
	}
}

func TestAlignDriftBeyondOneFrameNotMoved(t *testing.T) {
	hi := repWith(8, nil)
	hi.Representation = "high"
	low := repWith(8, func(i int) int64 {
		if i == 3 {
			return 7000 // > one 3000 frame
		}
		return 0
	})
	low.Representation = "low"
	rep := Align([]RepBoundaries{hi, low})
	if rep.Aligned != 7 || rep.NotSwitchable != 1 {
		t.Fatalf("want 7 aligned 1 failed, got %+v", rep)
	}
	bad := rep.Points[3]
	if bad.Switchable || bad.PTS90k != 3*90000 {
		t.Fatalf("reference boundary must stay put and be non-switchable: %+v", bad)
	}
}

func TestAlignSkipsIneligibleMetadataRep(t *testing.T) {
	ok := repWith(8, nil)
	ok.Representation = "high"
	bad := repWith(8, nil)
	bad.Representation = "low"
	bad.Eligible = false
	rep := Align([]RepBoundaries{ok, bad})
	if len(rep.Points[0].Members) != 1 {
		t.Fatalf("ineligible rep must be excluded, members=%d", len(rep.Points[0].Members))
	}
}
