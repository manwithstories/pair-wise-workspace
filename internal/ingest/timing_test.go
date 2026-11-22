package ingest

import (
	"testing"

	"mediarun/internal/mediaerr"
)

func TestMP4TrackRejectsDTSRegression(t *testing.T) {
	tr := &mp4Track{desc: TrackDesc{ID: "video/0"}}
	// First an advancing sample establishes the clock.
	if err := tr.validate(flatSample{dts: 100, duration: 10, offset: 0}); err != nil {
		t.Fatalf("first sample: %v", err)
	}
	// A backwards DTS must be a structured DTS_REGRESSION with track context.
	err := tr.validate(flatSample{dts: 90, duration: 10, offset: 64})
	if !mediaerr.Is(err, mediaerr.CodeDTSRegression) {
		t.Fatalf("want DTS_REGRESSION, got %v", err)
	}
	se := err.(*mediaerr.Error)
	if se.Track != "video/0" || se.Offset != 64 {
		t.Fatalf("error context wrong: %+v", se)
	}
}

func TestMP4TrackRejectsNegativeDuration(t *testing.T) {
	tr := &mp4Track{desc: TrackDesc{ID: "audio/1"}}
	err := tr.validate(flatSample{dts: 0, duration: -5, offset: 128})
	if !mediaerr.Is(err, mediaerr.CodeNegativeDuration) {
		t.Fatalf("want NEGATIVE_DURATION, got %v", err)
	}
	if se := err.(*mediaerr.Error); se.Track != "audio/1" {
		t.Fatalf("track context missing: %+v", se)
	}
}

func TestMP4TrackAcceptsMonotonicTimeline(t *testing.T) {
	tr := &mp4Track{desc: TrackDesc{ID: "video/0"}}
	for _, dts := range []int64{0, 1001, 2002, 3003} {
		if err := tr.validate(flatSample{dts: dts, duration: 1001, offset: dts}); err != nil {
			t.Fatalf("monotonic DTS %d rejected: %v", dts, err)
		}
	}
}
