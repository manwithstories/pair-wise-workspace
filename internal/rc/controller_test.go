package rc

import (
	"testing"

	"tcr/internal/codec"
	"tcr/internal/fixture"
	"tcr/internal/yuv"
)

func TestRCConvergence(t *testing.T) {
	dir := t.TempDir()
	m, err := fixture.Generate(dir, 6, 320, 240, 48, 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Segments {
		frames, err := yuv.LoadSegment(dir+"/"+s.File, 320, 240)
		if err != nil {
			t.Fatal(err)
		}
		hdr := codec.SegmentHeader{SegIndex: s.Index, FrameCount: len(frames), W: 320, H: 240, FPS: 25, TargetKbps: 900}
		target := int64(900) * 1000 * int64(len(frames)) / 25
		oc, err := EncodeSegment(frames, hdr, target, dir+"/out_"+s.File+".tcr")
		if err != nil {
			t.Fatal(err)
		}
		dev := float64(oc.Bits-target) / float64(target) * 100
		t.Logf("%s qp=%.2f bits=%d dev=%.2f%%", s.Name, oc.QPEff, oc.Bits, dev)
		if dev > 5 || dev < -5 {
			t.Errorf("%s deviation %.2f%% exceeds 5%%", s.Name, dev)
		}
	}
}
