package timeline

import (
	"testing"

	"github.com/pairwise/relay/internal/isobmff"
	"github.com/pairwise/relay/internal/media"
)

func frag(tfdt uint64, durations ...uint32) *isobmff.Fragment {
	f := &isobmff.Fragment{TFDT: tfdt}
	for _, d := range durations {
		f.Samples = append(f.Samples, isobmff.Sample{Duration: d, Size: 1})
	}
	return f
}

func TestAlignRepairsProvableTFDTFallback(t *testing.T) {
	in := TrackInput{Rendition: "high", Timescale: 90000, Fragments: []FragmentAt{
		{Segment: 1, Frag: frag(0, 90000, 90000)},
		{Segment: 2, Frag: frag(180000, 90000, 90000)},
		// Fell back one whole segment, but two positive durations prove 180k.
		{Segment: 3, Frag: frag(180000, 90000, 90000)},
	}}
	rep, err := Align([]TrackInput{in}, 3, 180000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Aligned != 3 {
		t.Fatalf("aligned=%d/3 faults=%v", rep.Aligned, rep.Faults)
	}
	c := rep.Boundaries[2].Cells[0]
	if !c.Repaired || c.Start90k != 360000 {
		t.Fatalf("cell=%+v", c)
	}
	if len(rep.Discontinuities) != 0 {
		t.Fatalf("unexpected discontinuities: %v", rep.Discontinuities)
	}
}

func TestUnprovableFallbackOpensCommonDiscontinuity(t *testing.T) {
	in := TrackInput{Rendition: "high", Timescale: 90000, Fragments: []FragmentAt{
		{Segment: 1, Frag: frag(0, 180000)},
		{Segment: 2, Frag: frag(180000, 180000)},
		{Segment: 3, Frag: frag(180000, 90000)}, // duration does not prove segment
	}}
	rep, err := Align([]TrackInput{in}, 3, 180000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Aligned != 3 {
		t.Fatalf("grid reset should still align boundary: %d/3", rep.Aligned)
	}
	if len(rep.Discontinuities) != 1 || rep.Discontinuities[0] != 3 {
		t.Fatalf("discontinuities=%v", rep.Discontinuities)
	}
	if rep.Boundaries[2].Faults == nil {
		t.Fatal("common boundary lacks fault")
	}
}

func TestThreeTimescalesShareBoundaryWithinOneTick(t *testing.T) {
	mk := func(name string, ts uint32, segDurTrack uint64, sample uint32) TrackInput {
		track := TrackInput{Rendition: name, Timescale: ts}
		for i := uint64(1); i <= 3; i++ {
			track.Fragments = append(track.Fragments, FragmentAt{
				Segment: i,
				Frag:    frag((i-1)*segDurTrack, sample, sample, sample),
			})
		}
		return track
	}
	// 3 samples each; track durations 180000, 96000 and 88200 all map to 180000.
	rep, err := Align([]TrackInput{
		mk("high", 90000, 180000, 60000),
		mk("mid", 48000, 96000, 32000),
		mk("low", 44100, 88200, 29400),
	}, 3, 180000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Aligned != 3 {
		t.Fatalf("aligned=%d/3", rep.Aligned)
	}
	for _, b := range rep.Boundaries {
		for _, c := range b.Cells {
			if !media.WithinOneTick(c.Start90k, b.Start90k) {
				t.Fatalf("tick mismatch: %+v vs %d", c, b.Start90k)
			}
		}
	}
}
