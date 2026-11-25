package segment

import (
	"testing"

	"streamcut/internal/media"
)

// idrUnit builds a video unit whose Annex B payload starts with an IDR NAL.
func idrUnit(pts media.Time90) Unit {
	return Unit{AU: media.AccessUnit{
		Kind: media.KindVideoH264, PTS: pts, DTS: pts, HasPTS: true, HasDTS: true,
		Data: []byte{0, 0, 0, 1, 0x65, 0xAA, 0xBB},
	}}
}

// pUnit builds a non-IDR (P-frame) video unit.
func pUnit(pts media.Time90) Unit {
	return Unit{AU: media.AccessUnit{
		Kind: media.KindVideoH264, PTS: pts, DTS: pts, HasPTS: true, HasDTS: true,
		Data: []byte{0, 0, 0, 1, 0x21, 0xCC},
	}}
}

func audioUnit(pts media.Time90) Unit {
	return Unit{AU: media.AccessUnit{
		Kind: media.KindAudioPCM16LE, PTS: pts, HasPTS: true, Data: []byte{1, 2},
	}}
}

// TestCutsAtIDRWithOneGOPError feeds a 10-frame/GOP, 1-second GOP stream and
// asks for 2-second segments: every boundary must land on an IDR and no more
// than one GOP away from the target.
func TestCutsAtIDRWithOneGOPError(t *testing.T) {
	const fps = media.Time90(3000) // 90000/30 ticks per frame
	target := media.TicksPerSecond * 2
	c := NewCutter(target)

	var done []*Segment
	gop := media.Time90(30) // 30 frames per GOP
	for f := 0; f < 300; f++ { // 10 seconds
		pts := media.Time90(f) * fps
		var u Unit
		if f%int(gop) == 0 {
			u = idrUnit(pts)
		} else {
			u = pUnit(pts)
		}
		if s := c.Push(u); s != nil {
			done = append(done, s)
		}
		if s := c.Push(audioUnit(pts)); s != nil {
			done = append(done, s)
		}
	}
	if s := c.Flush(); s != nil {
		done = append(done, s)
	}
	if len(done) != 5 {
		t.Fatalf("segments = %d, want 5", len(done))
	}
	for i, s := range done {
		if !s.Keyframe {
			t.Fatalf("segment %d must start at an IDR", i)
		}
		err := s.Duration() - target
		if err < 0 {
			err = -err
		}
		if err > media.TicksPerSecond { // one GOP tolerance
			t.Fatalf("segment %d boundary error %v exceeds one GOP", i, err)
		}
	}
}

// TestCCGapMarksSegmentUnpublishable verifies that a CC jump inside one
// segment only flags that segment; neighbors remain publishable.
func TestCCGapMarksSegmentUnpublishable(t *testing.T) {
	const fps = media.Time90(3000)
	c := NewCutter(media.TicksPerSecond * 2)
	var done []*Segment
	for f := 0; f < 150; f++ { // 5 s, 2 full + 1 partial segment
		pts := media.Time90(f) * fps
		u := pUnit(pts)
		if f%30 == 0 {
			u = idrUnit(pts)
		}
		// Inject a CC jump at frame 75 (inside the second segment).
		if f == 75 {
			u.CCError = true
		}
		if s := c.Push(u); s != nil {
			done = append(done, s)
		}
	}
	if s := c.Flush(); s != nil {
		done = append(done, s)
	}
	if len(done) != 3 {
		t.Fatalf("segments = %d, want 3", len(done))
	}
	if done[0].HasCCGap || !done[0].Publishable {
		t.Fatal("segment 0 must be clean and publishable")
	}
	if !done[1].HasCCGap || done[1].Publishable {
		t.Fatal("segment 1 must carry the CC gap and be unpublishable")
	}
	if done[2].HasCCGap || !done[2].Publishable {
		t.Fatal("segment 2 must be clean and publishable")
	}
}

// TestClockGapFlagsWithoutFabricatingTime ensures a clock-gap unit is marked
// and the segment is withheld rather than assigned an invented timestamp.
func TestClockGapFlagsWithoutFabricatingTime(t *testing.T) {
	c := NewCutter(media.TicksPerSecond)
	c.Push(idrUnit(0))
	u := pUnit(3000)
	u.ClockGap = true
	c.Push(u)
	s := c.Flush()
	if s == nil || s.Publishable || !s.HasClockGap {
		t.Fatalf("clock-gap segment must be unpublishable: %+v", s)
	}
}
