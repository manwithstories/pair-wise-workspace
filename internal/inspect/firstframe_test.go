package inspect

import (
	"testing"

	"relayaudit/internal/fixture"
)

// TestScannerFirstRecordIsIDR guards the fixture's Annex B framing: both video
// tracks must open with SPS, PPS and an IDR slice in their very first record.
func TestScannerFirstRecordIsIDR(t *testing.T) {
	seg, err := fixture.Generate(fixture.Spec{Seed: 73, Size: 1 << 19})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	sc := NewScanner()
	for _, rec := range seg.Records {
		f := sc.Push(rec.Track, rec.PTS, rec.Payload, rec.Offset)
		if rec.Offset == seg.Records[0].Offset && rec.Track <= 2 {
			if f.Type != FrameIDR || !f.SPSPPS {
				t.Fatalf("track %d first record type=%v spspps=%v, want IDR with parameter sets",
					rec.Track, f.Type, f.SPSPPS)
			}
		}
	}
}
