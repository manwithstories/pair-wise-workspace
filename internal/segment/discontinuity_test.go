package segment

import (
	"testing"

	"github.com/archive/tsrestore/internal/rtp"
	"github.com/archive/tsrestore/internal/sample"
	"github.com/archive/tsrestore/internal/timeline"
)

// buildPacketsFromFrames turns TS packets into ordered RTP packets, skipping
// any frame indices listed in dropped so the segmenter loses an IDR.
func buildPacketsFromFrames(t *testing.T, r sample.Rendition, nFrames int, dropped map[int]bool) []*rtp.Packet {
	t.Helper()
	var seq uint16
	var out []*rtp.Packet
	cc := &sample.FrameCC{}
	for fi := 0; fi < nFrames; fi++ {
		if dropped[fi] {
			cc.Video = (cc.Video + uint8(r.TSPerFrame)) & 0x0F
			continue
		}
		fr := sample.BuildTestFrame(fi, r.TSPerFrame, cc)
		for k := 0; k < len(fr.TSPackets); k += tsPerRTP {
			end := k + tsPerRTP
			if end > len(fr.TSPackets) {
				end = len(fr.TSPackets)
			}
			var payload []byte
			for _, p := range fr.TSPackets[k:end] {
				payload = append(payload, p...)
			}
			out = append(out, &rtp.Packet{
				PayloadType: 33, Sequence: seq, Timestamp: fr.RTPTime,
				Marker: true, Payload: payload,
			})
			seq++
		}
	}
	return out
}

const tsPerRTP = 7

func TestMissingIDRMarksDiscontinuity(t *testing.T) {
	r := sample.DefaultRenditions()[0]
	// 18 seconds: segment boundaries at 6 s (frame 150) and 12 s (frame 300).
	// Drop the IDR at frame 150 so the first cut must overshoot to frame 200
	// (the next 2-second IDR) and flag a discontinuity.
	dropped := map[int]bool{sample.SegmentFrames: true}
	packets := buildPacketsFromFrames(t, r, sample.ProgramDuration(18), dropped)

	clock := timeline.NewClock(timeline.RTPClockRate)
	b := NewBuilder(r.Name, clock)
	var segs []*Segment
	for _, p := range packets {
		made, err := b.Push(p)
		if err != nil {
			t.Fatal(err)
		}
		segs = append(segs, made...)
	}
	if tail := b.Flush(); tail != nil {
		segs = append(segs, tail)
	}
	if len(segs) < 2 {
		t.Fatalf("expected at least 2 segments, got %d", len(segs))
	}
	// The first full segment overshot its ideal boundary because the 6 s IDR
	// was lost, so it must carry a discontinuity while remaining key-aligned.
	first := segs[0]
	if !first.Discontinuity {
		t.Fatal("segment past a missing neighboring IDR must be marked discontinuity")
	}
	if !first.KeyAligned {
		t.Fatal("cut must still land on a safe IDR")
	}
	// It cut at frame 200 (8 s), not the ideal 6 s grid.
	if d := first.DurationSecs - 8.0; d > 0.05 || d < -0.05 {
		t.Fatalf("overshoot segment duration = %.3f, want ~8.0", first.DurationSecs)
	}
}

func TestExactGridHasNoDiscontinuity(t *testing.T) {
	r := sample.DefaultRenditions()[0]
	packets := buildPacketsFromFrames(t, r, sample.ProgramDuration(12), nil)
	clock := timeline.NewClock(timeline.RTPClockRate)
	b := NewBuilder(r.Name, clock)
	var segs []*Segment
	for _, p := range packets {
		made, err := b.Push(p)
		if err != nil {
			t.Fatal(err)
		}
		segs = append(segs, made...)
	}
	for i, s := range segs {
		if s.Discontinuity {
			t.Fatalf("clean grid segment %d must not be discontinuous", i)
		}
	}
}
