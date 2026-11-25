package delivery

import (
	"testing"

	"cinepipe/internal/index"
	"cinepipe/internal/synth"
)

func TestProbeCaptureSamples(t *testing.T) {
	s := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))
	idx, units := index.RunWithSamples(s.Data)
	t.Logf("originTicks=%d units=%d", idx.OriginTicks, len(units))
	for i := 0; i < 3 && i < len(units); i++ {
		t.Logf("unit %d ticks=%d rel=%d key=%v data=%d", i, units[i].Ticks, units[i].Ticks-idx.OriginTicks, units[i].Keyframe, len(units[i].SampleData))
	}
	segs := planSegments(idx, int64(90000)*4)
	for i, sg := range segs {
		t.Logf("seg %d start=%d end=%d frames=%d", i, sg.startTicks, sg.endTicks, sg.frames)
	}
}
