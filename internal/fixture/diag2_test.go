package fixture_test

import (
	"testing"

	"avpipe/internal/fixture"
	"avpipe/internal/media"
)

func TestDiagIdent(t *testing.T) {
	frames := fixture.GenerateFrames(60, 41)
	audio := fixture.GenerateAudio(16, 41)
	canonical := fixture.BuildPackets(frames, audio)
	ids := map[[4]uint32]int{}
	for _, p := range canonical {
		k := [4]uint32{uint32(p.Kind), uint32(p.ParityOf), p.BlockID, p.Seq}
		ids[k]++
		if ids[k] > 1 {
			t.Errorf("collision %v count=%d", k, ids[k])
		}
	}
	// Show parity identities
	n := 0
	for _, p := range canonical {
		if p.Kind == media.KindParity {
			n++
			t.Logf("parity kind=%d parityOf=%d block=%d seq=%d", p.Kind, p.ParityOf, p.BlockID, p.Seq)
		}
	}
	t.Logf("total parity=%d", n)
}
