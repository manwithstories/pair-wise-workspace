package repair

import (
	"testing"

	"archivecheck/internal/clock"
	"archivecheck/internal/parse"
)

func TestCrossTrackMappingUsesExactRationals(t *testing.T) {
	init := &parse.Init{Segment: "init.mp4", Tracks: map[uint32]*parse.Track{
		1: {ID: 1, Timescale: 90000, Handler: "vide"},
		2: {ID: 2, Timescale: 48000, Handler: "soun"},
	}}
	frags := []*parse.Fragment{
		{Segment: "v1", Sequence: 1, HasMfhd: true,
			Trafs: []parse.Traf{{TrackID: 1, BaseDecodeTime: 0, Samples: []parse.Sample{
				{TrackID: 1, Duration: 3000, Size: 10, Offset: 0},
			}}}},
		{Segment: "a1", Sequence: 1, HasMfhd: true,
			Trafs: []parse.Traf{{TrackID: 2, BaseDecodeTime: 0, Samples: []parse.Sample{
				{TrackID: 2, Duration: 1024, Size: 10, Offset: 10},
			}}}},
	}
	tl, err := Build(0, frags[:1], init.Tracks[1])
	if err != nil {
		t.Fatal(err)
	}
	m, err := clock.NewMapping(clock.MustNew(90000), clock.MustNew(48000))
	if err != nil {
		t.Fatal(err)
	}
	for _, ft := range tl.Fragments {
		if ft.TrackID != 1 {
			continue
		}
		for _, s := range ft.Samples {
			got, exact, err := m.Map(s.EndDTS)
			if err != nil {
				t.Fatal(err)
			}
			want, err := clock.MustNew(90000).Rescale(s.EndDTS, clock.MustNew(48000))
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("mapped %d != %d", got, want)
			}
			if exact.Sign() < 0 {
				t.Fatalf("negative exact mapping: %s", exact.RatString())
			}
		}
	}
	// 3000 ticks at 90 kHz == 1600 ticks at 48 kHz exactly.
	mapped, exact, _ := m.Map(3000)
	if mapped != 1600 || exact.IsInt() != true {
		t.Fatalf("mapped=%d exact=%s", mapped, exact.RatString())
	}
}
