package audio

import (
	"math"
	"testing"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// tone builds an interleaved sine of the given duration.
func tone(rate, ch int, seconds float64, freq, amp float64) *Material {
	frames := int(seconds * float64(rate))
	s := make([]float64, frames*ch)
	for i := 0; i < frames; i++ {
		v := amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))
		for c := 0; c < ch; c++ {
			s[i*ch+c] = v
		}
	}
	return &Material{Track: "a", Rate: rate, Layout: Stereo, Span: timeline.Span{Start: 0, End: timeline.Micros(seconds * 1e6)}, Samples: s}
}

func TestNormalizeResamplesAndPreservesDuration(t *testing.T) {
	m := tone(44100, 2, 1.0, 440, 0.3)
	res, err := Normalize(m, DefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resampled {
		t.Fatal("44.1k -> 48k must report resampling")
	}
	if res.Rate != 48000 {
		t.Fatalf("rate = %d", res.Rate)
	}
	wantFrames := 48000
	if got := res.Frames; got != wantFrames {
		t.Fatalf("frames = %d, want %d", got, wantFrames)
	}
	// Boundaries are preserved: the output span is unchanged.
	if res.Span != m.Span {
		t.Fatalf("span changed: %+v -> %+v", m.Span, res.Span)
	}
}

func TestNormalizeUpmixesMonoToStereo(t *testing.T) {
	frames := 480
	s := make([]float64, frames)
	for i := range s {
		s[i] = 0.2 * math.Sin(2*math.Pi*220*float64(i)/48000)
	}
	m := &Material{Track: "a", Rate: 48000, Layout: Mono, Span: timeline.Span{Start: 0, End: 10_000}, Samples: s}
	res, err := Normalize(m, DefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upmixed || res.Layout != Stereo {
		t.Fatalf("upmix not reported: %+v", res)
	}
	if got := len(res.Material.Samples); got != frames*2 {
		t.Fatalf("interleaved length = %d, want %d", got, frames*2)
	}
}

func TestNormalizeSilenceIsFlagged(t *testing.T) {
	m := &Material{Track: "a", Rate: 48000, Layout: Stereo, Span: timeline.Span{Start: 0, End: 1000},
		Samples: make([]float64, 960*2)}
	res, err := Normalize(m, DefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Silent {
		t.Fatal("digital silence must be flagged")
	}
	found := false
	for _, s := range res.Status {
		if s.Code == core.CodeSilentAudio {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing silent status: %+v", res.Status)
	}
	if res.GainDB != 0 {
		t.Fatalf("silence must not be gained: %f", res.GainDB)
	}
}

func TestNormalizeClippingIsFlagged(t *testing.T) {
	m := tone(48000, 2, 0.1, 440, 1.0)
	res, err := Normalize(m, DefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Clipped {
		t.Fatal("full-scale input must be flagged as clipped")
	}
	// The limiter must keep the deliverable within full scale.
	if res.OutputPeakDB > 0.01 {
		t.Fatalf("output peak exceeds full scale: %.3f dB", res.OutputPeakDB)
	}
}

func TestNormalizeEmptyTrackIsStatusNotError(t *testing.T) {
	m := &Material{Track: "a", Rate: 48000, Layout: Stereo, Span: timeline.Span{Start: 0, End: 0}}
	res, err := Normalize(m, DefaultTarget())
	if err != nil {
		t.Fatalf("empty track must be a status, not an error: %v", err)
	}
	if !res.Silent {
		t.Fatal("empty track must report silent")
	}
	found := false
	for _, s := range res.Status {
		if s.Code == core.CodeEmptyTrack {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing empty status: %+v", res.Status)
	}
}

func TestNormalizeRejectsUnsetRate(t *testing.T) {
	m := &Material{Track: "a", Rate: 0, Layout: Stereo, Span: timeline.Span{Start: 0, End: 1}, Samples: []float64{0.1}}
	if _, err := Normalize(m, DefaultTarget()); err == nil {
		t.Fatal("unset sample rate must be rejected, never guessed")
	}
}

func TestNormalizeLoudnessMovesTowardTarget(t *testing.T) {
	m := tone(48000, 2, 1.0, 440, 0.05)
	res, err := Normalize(m, DefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	after := loudness(res.Material.Samples, res.Material.Layout)
	if math.Abs(after-DefaultTarget().LoudnessLUFS) > 1.0 {
		t.Fatalf("loudness %.2f not near target %.2f", after, DefaultTarget().LoudnessLUFS)
	}
}

func TestNormalizeDoesNotExceedSpanBoundary(t *testing.T) {
	m := tone(44100, 2, 0.5, 440, 0.3)
	res, err := Normalize(m, DefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	spanFrames := float64(res.Span.Duration()) / 1e6 * float64(res.Rate)
	if math.Abs(float64(res.Frames)-spanFrames) > 1 {
		t.Fatalf("frames %d drift from span %0.f", res.Frames, spanFrames)
	}
}

func TestNormalizeRejectsUnsupportedLayout(t *testing.T) {
	m := &Material{Track: "a", Rate: 48000, Layout: Layout("7.1"), Span: timeline.Span{Start: 0, End: 1}, Samples: []float64{0.1}}
	if _, err := Normalize(m, DefaultTarget()); err == nil {
		t.Fatal("unknown layout must be rejected")
	}
}
