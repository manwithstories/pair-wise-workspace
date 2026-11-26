package fixture

import (
	"testing"

	"github.com/pairwise/relay/internal/isobmff"
	"github.com/pairwise/relay/internal/media"
)

func TestGeneratedFragmentsParseAndMFHDJumpCount(t *testing.T) {
	in := Generate(Config{Seed: 17, Rungs: 3, Segments: 24, FaultSuite: true})
	if len(in.RungInputs) != 3 {
		t.Fatalf("rungs=%d", len(in.RungInputs))
	}
	for _, rung := range in.RungInputs {
		var last uint32
		have := false
		jumps := 0
		for seg := uint64(1); seg <= 24; seg++ {
			f, fault := isobmff.ParseFragment(rung.Intact[seg], media.FragmentID{Rendition: rung.ID, Segment: seg})
			if fault != nil {
				t.Fatalf("%s seg%d: %v", rung.ID, seg, fault)
			}
			if have && f.MFHDSequence != last+1 {
				jumps++
			}
			last = f.MFHDSequence
			have = true
		}
		if jumps != 1 {
			t.Fatalf("rung %s mfhd jumps=%d want 1", rung.ID, jumps)
		}
	}
}
