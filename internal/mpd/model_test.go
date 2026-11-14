package mpd

import "testing"

func TestRoundTrip(t *testing.T) {
	one, zero := int64(1), int64(0)
	m := &MPD{
		Profiles:                  "urn:mpeg:dash:profile:isoff-live:2011",
		Type:                      "static",
		MediaPresentationDuration: FormatDuration(600),
		MinBufferTime:             "PT2S",
		Periods: []Period{{
			ID: "p0", Start: "PT0S",
			AdaptationSets: []AdaptationSet{{
				ID: 1, MimeType: "video/mp4", ContentType: "video",
				Representations: []Representation{{
					ID: "r1", Bandwidth: 400000, Width: 640, Height: 360,
					BaseURL: "r1/",
					SegmentTemplate: SegmentTemplate{
						Timescale:              90000,
						PresentationTimeOffset: 1665000,
						Media:                  "$Number%05d$.m4s",
						StartNumber:            &one,
						SegmentTimeline: &SegmentTimeline{S: []S{
							{T: &one, D: 360000, R: &zero},
							{D: 360000},
						}},
					},
				}},
			}},
		}},
	}
	if err := CheckRoundTrip(m); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	data, err := Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := back.Periods[0].AdaptationSets[0].Representations[0].SegmentTemplate.Timescale; got != 90000 {
		t.Fatalf("timescale = %d, want 90000", got)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[int64]string{0: "PT0S", 4: "PT4S", 600: "PT10M", 3661: "PT1H1M1S", 3600: "PT1H"}
	for in, want := range cases {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%d) = %q, want %q", in, got, want)
		}
	}
}
