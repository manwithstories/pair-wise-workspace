package fixture

import "testing"

func TestDbgWrap2(t *testing.T) {
	spec := DefaultSpec()
	spec.Pattern = PatternWrap
	vsegs := videoSegments(spec.VideoSamples)
	deltas, inj := buildVideoDeltas(spec, vsegs)
	var cum uint32
	for i, d := range deltas {
		if i >= 58 && i <= 61 {
			t.Logf("i=%d dts=%d (0x%08x) delta=%d inj=%d", i, cum, cum, d, inj[i])
		}
		cum += d
	}
}
