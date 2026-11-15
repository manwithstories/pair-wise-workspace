package mpd

import (
	"math/big"
	"testing"

	"dashkit/internal/model"
)

func u64(v uint64) *uint64 { return &v }

func sampleMPD() *model.MPD {
	return &model.MPD{
		ID: "urn:uuid:test", Type: "static", MediaPresentationDuration: big.NewRat(120, 1),
		Periods: []*model.Period{{ID: "p0", Start: big.NewRat(0, 1), Sets: []*model.AdaptationSet{
			{ID: 1, ContentType: "video", Reps: []*model.Representation{
				{ID: "v1", Bandwidth: 300000, Width: 426, Height: 240, FrameRate: "30000/1001",
					Codecs: "avc1.42E01E", Template: &model.SegmentTemplate{
						Timescale: 90000, PresentationTimeOffset: new(big.Rat), StartNumber: 1,
						Media: "$RepresentationID$/seg_$Number%05d$.m4s", Initialization: "$RepresentationID$/init.mp4",
						Timeline: &model.SegmentTimeline{S: []*model.S{{T: u64(3003), D: 180180, R: 28}, {D: 174780}}},
					}},
			}},
		}}},
	}
}

func TestRoundTrip(t *testing.T) {
	m := sampleMPD()
	data, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if probs := Compare(m, back); len(probs) != 0 {
		t.Fatalf("round trip mismatch: %v", probs)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[string]string{
		"0/1":       "PT0S",
		"60/1":      "PT60S",
		"2002/1000": "PT2.002S",
		"1/2":       "PT0.5S",
	}
	for in, want := range cases {
		r, _ := new(big.Rat).SetString(in)
		if got := formatDuration(r); got != want {
			t.Fatalf("formatDuration(%s)=%s want %s", in, got, want)
		}
		back, err := parseDuration(want)
		if err != nil || back.Cmp(r) != 0 {
			t.Fatalf("parseDuration(%s)=%v,%v", want, back, err)
		}
	}
}

func TestCompareDetectsMismatch(t *testing.T) {
	a := sampleMPD()
	b := sampleMPD()
	b.Periods[0].Sets[0].Reps[0].Template.Timeline.S[0].D = 180181
	if probs := Compare(a, b); len(probs) == 0 {
		t.Fatal("expected mismatch")
	}
}
