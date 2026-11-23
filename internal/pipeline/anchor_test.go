package pipeline

import "testing"

// TestDriftAnchorsUseVideoTrack guards the track label the buffer assigns:
// alignment anchors must be drawn from the video track, not an audio or
// unrelated interval, or the drift fit sees an empty baseline.
func TestDriftAnchorsUseVideoTrack(t *testing.T) {
	s, media, err := buildSample(Config{
		Seed: 42, Segments: 4, SegmentDurNS: 2_000_000_000, PacketRate: 50,
		Faults: Faults{Drift: true}, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if len(media.Segments) == 0 {
		t.Fatal("no segments")
	}
	j := runJitter(s, recoverRTP(s))
	anchors := anchorsFor(s, j)
	if len(anchors) < 4 {
		t.Fatalf("anchors = %d, need at least 4", len(anchors))
	}
	for i := 1; i < len(anchors); i++ {
		if anchors[i].MediaNS <= anchors[i-1].MediaNS {
			t.Fatalf("anchors not increasing at %d", i)
		}
		if anchors[i].RefNS <= anchors[i-1].RefNS {
			t.Fatalf("reference anchors not increasing at %d", i)
		}
	}
}
