package pipeline_test

import (
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/pipeline"
)

func TestDemoBuildsEightAlignedSegments(t *testing.T) {
	specs := fixture.DemoRenditionSpecs()
	var data [2][]byte
	for i, sp := range specs {
		b, err := fixture.CleanRendition(73021, sp)
		if err != nil {
			t.Fatal(err)
		}
		data[i] = b
		t.Logf("rendition=%s bytes=%d packets=%d", sp.ID, len(b), len(b)/188)
	}
	built, err := pipeline.Build(73021, specs, data[:])
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Alignment.Segments) != 8 {
		t.Fatalf("segments=%d want 8", len(built.Alignment.Segments))
	}
	t.Logf("max_skew_ms=%v", built.Alignment.MaxSkewMS)
	for bi, b := range built.Alignment.Boundaries {
		if bi > 8 {
			break
		}
		for r := range specs {
			for _, f := range built.Tracks[r].Frames {
				if f.Keyframe && f.PTS == b.RenditionPTS[r] {
					t.Logf("boundary=%d r=%d pts=%d offset=%d packet=%d", bi, r, f.PTS, f.Offset, f.Offset/188)
					break
				}
			}
		}
	}
}
