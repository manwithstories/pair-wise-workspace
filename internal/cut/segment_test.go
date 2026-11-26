package cut

import (
	"testing"

	"mediapipe/internal/media"
)

func framePackets() []media.Packet {
	tb := media.NewRat(1, 30)
	var ps []media.Packet
	// Three 10-second target cuts over 25 seconds. Keyframes every 2 seconds.
	for f := 0; f < 25*30; f++ {
		t := int64(f)
		ps = append(ps, media.Packet{
			Sequence: uint64(f*2 + 1), StreamIndex: 0, PTS: t, DTS: t, TimeBase: tb,
			Key: f%60 == 0,
		})
		// Audio packets at the same frame grid in this deterministic fixture.
		ps = append(ps, media.Packet{
			Sequence: uint64(f*2 + 2), StreamIndex: 1, PTS: t, DTS: t, TimeBase: tb,
		})
	}
	return ps
}

func TestPlanSegmentsTargetsAndBoundaryReencode(t *testing.T) {
	res, err := PlanSegments(framePackets(), Options{TargetSeconds: 10, VideoStream: 0, AudioStream: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 3 {
		t.Fatalf("segments = %d, want 3: %+v", len(res.Segments), res.Segments)
	}
	if res.Segments[0].ReencodeBoundary {
		t.Fatal("first segment should start on keyframe")
	}
	// At exactly 10s and 20s the fixture has keyframes, so all are aligned.
	for _, s := range res.Segments {
		if !s.KeyFrameAligned || !s.WithinOneFrame {
			t.Fatalf("segment not aligned: %+v", s)
		}
	}
}

func TestNonKeyframeTargetMarksReencodePrefix(t *testing.T) {
	ps := framePackets()
	// Shift target to 11 seconds: previous keyframe is 10s.
	res, err := PlanSegments(ps, Options{TargetSeconds: 11, VideoStream: 0, AudioStream: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) < 2 {
		t.Fatalf("segments = %d", len(res.Segments))
	}
	seg := res.Segments[1]
	if !seg.ReencodeBoundary || seg.BoundaryFrames != 30 {
		t.Fatalf("expected 30 boundary frames from 10s to 11s, got %+v", seg)
	}
}

func TestAvDriftBeyondFrameRejected(t *testing.T) {
	ps := framePackets()
	for i := range ps {
		if ps[i].StreamIndex == 1 && ps[i].PTS == 300 {
			ps[i].PTS += 2 // 2/30s = 66.7ms > one 33.3ms frame
			break
		}
	}
	_, err := PlanSegments(ps, Options{TargetSeconds: 10, VideoStream: 0, AudioStream: 1})
	if err == nil {
		t.Fatal("expected A/V drift rejection")
	}
}
