package clock

import (
	"errors"
	"testing"
)

func TestAudioSamplesToMedia(t *testing.T) {
	// 20 ms of 48 kHz audio = 960 samples = 1800 media ticks.
	if got := SamplesToMedia(960, 48000); got != 1800 {
		t.Fatalf("SamplesToMedia(960, 48000) = %d, want 1800", got)
	}
}

func TestToMediaConvertsRates(t *testing.T) {
	tl := New()
	if err := tl.Register("audio", 48000); err != nil {
		t.Fatal(err)
	}
	if err := tl.Register("video", 90000); err != nil {
		t.Fatal(err)
	}
	a, err := tl.ToMedia("audio", 96000) // 2 s of audio
	if err != nil {
		t.Fatal(err)
	}
	v, err := tl.ToMedia("video", 180000) // 2 s of video
	if err != nil {
		t.Fatal(err)
	}
	if a != v || a != 180000 {
		t.Fatalf("audio=%d video=%d, want both 180000", a, v)
	}
}

func TestUnwrapAcrossWraparound(t *testing.T) {
	tl := New()
	if err := tl.Register("video", 90000); err != nil {
		t.Fatal(err)
	}
	// Start near the 32-bit boundary and cross it.
	seq := []uint32{0xFFFFFFF0, 0xFFFFFFF8, 0x00000004, 0x00000010}
	want := []int64{0xFFFFFFF0, 0xFFFFFFF8, 0x100000004, 0x100000010}
	for i, raw := range seq {
		got, err := tl.Unwrap("video", raw)
		if err != nil {
			t.Fatal(err)
		}
		if got != want[i] {
			t.Fatalf("unwrap[%d] = %#x, want %#x", i, got, want[i])
		}
	}
}

func TestUnknownTrackError(t *testing.T) {
	tl := New()
	_, err := tl.ToMedia("ghost", 0)
	var ute *UnknownTrackError
	if !errors.As(err, &ute) {
		t.Fatalf("err = %v, want UnknownTrackError", err)
	}
	if ute.Track != "ghost" {
		t.Fatalf("error carries track %q, want ghost", ute.Track)
	}
}

func TestInvalidTimebaseError(t *testing.T) {
	tl := New()
	for _, rate := range []uint32{0, 90001} {
		err := tl.Register("bad", rate)
		var ite *InvalidTimebaseError
		if !errors.As(err, &ite) {
			t.Fatalf("rate %d: err = %v, want InvalidTimebaseError", rate, err)
		}
	}
}

func TestDriftCorrectionAboveThreshold(t *testing.T) {
	d := NewDriftEstimator(DefaultDriftThresholdPPM, 4)
	// Track runs 1000 ppm fast: over each 900-tick reference span it
	// advances 900.9 ticks (use integers: 901).
	for i := 0; i < 8; i++ {
		ref := int64(i) * 225
		track := int64(i) * 225 * 1001 / 1000
		d.Observe("video", ref, track)
	}
	corr := d.Corrections("video")
	if len(corr) == 0 {
		t.Fatal("expected corrections for 1000 ppm drift")
	}
	for _, c := range corr {
		if c.PPM < DefaultDriftThresholdPPM {
			t.Fatalf("correction ppm %v below threshold", c.PPM)
		}
		if c.OffsetTicks >= 0 {
			t.Fatalf("fast track needs negative offset, got %d", c.OffsetTicks)
		}
	}
}

func TestDriftBelowThresholdNotCorrected(t *testing.T) {
	d := NewDriftEstimator(DefaultDriftThresholdPPM, 4)
	for i := 0; i < 8; i++ {
		ref := int64(i) * 90000
		track := ref * 10001 / 10000 // 100 ppm
		d.Observe("audio", ref, track)
	}
	if got := d.Corrections("audio"); len(got) != 0 {
		t.Fatalf("100 ppm drift corrected: %+v", got)
	}
}
