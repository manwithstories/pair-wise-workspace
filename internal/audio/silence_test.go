package audio

import (
	"errors"
	"testing"
	"time"

	"mediacq/internal/clock"
	"mediacq/internal/ingest"
	"mediacq/internal/testdata"
)

func framesFromTrack(t *testing.T, track *ingest.Track) []Frame {
	t.Helper()
	out := make([]Frame, 0, len(track.Samples))
	for _, s := range track.Samples {
		out = append(out, Frame{
			Time: s.Start, DurNS: s.Duration.Nanoseconds(), Payload: s.Payload,
		})
	}
	return out
}

func TestDetectFixtureSilences(t *testing.T) {
	f, err := testdata.Build(testdata.DefaultParams(17))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ingest.Parse(f.MP4)
	if err != nil {
		t.Fatal(err)
	}
	var audioTrack *ingest.Track
	for _, tr := range c.Tracks {
		if tr.Kind == "audio" {
			audioTrack = tr
		}
	}
	if audioTrack == nil {
		t.Fatal("no audio track")
	}
	cfg := Config{AmplitudeThreshold: 0.01, MinDurationNS: int64(80 * time.Millisecond), BytesPerFrame: 4}
	got, err := Detect(framesFromTrack(t, audioTrack), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected silence spans from fixture")
	}
	// Output must be on the unified timeline and match ground-truth count.
	if len(got) != len(f.ExpectedSilences) {
		t.Fatalf("silence spans = %d, ground truth = %d", len(got), len(f.ExpectedSilences))
	}
	for i, s := range got {
		want := f.ExpectedSilences[i]
		if s.Range.Start != want.Start || s.Range.End != want.End {
			t.Fatalf("span %d = %+v, want %+v", i, s.Range, want)
		}
		if s.Samples <= 0 {
			t.Fatal("span without samples")
		}
	}
}

func TestDetectMinDurationFilter(t *testing.T) {
	frames := []Frame{
		{Time: 0, DurNS: int64(10 * time.Millisecond), Payload: []byte{0, 0, 0, 0}},
		{Time: clock.Time(10 * time.Millisecond), DurNS: int64(10 * time.Millisecond), Payload: []byte{0, 0, 0, 0}},
		{Time: clock.Time(20 * time.Millisecond), DurNS: int64(10 * time.Millisecond), Payload: []byte{0x7f, 0, 0, 0}},
	}
	// Threshold filter: 20 ms of silence is below a 50 ms minimum.
	spans, err := Detect(frames, Config{AmplitudeThreshold: 0.01, MinDurationNS: int64(50 * time.Millisecond), BytesPerFrame: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 0 {
		t.Fatalf("short silence should be filtered, got %d spans", len(spans))
	}
}

func TestDetectMissingAudioIsQualityError(t *testing.T) {
	_, err := Detect(nil, DefaultConfig())
	if !errors.Is(err, ErrNoAudio) {
		t.Fatalf("want ErrNoAudio, got %v", err)
	}
}

func TestDetectLoudFrameNotSilent(t *testing.T) {
	// Little-endian 32-bit value 0x40000000, well above the 1% threshold.
	frames := []Frame{
		{Time: 0, DurNS: int64(10 * time.Millisecond), Payload: []byte{0x00, 0x00, 0x00, 0x40}},
	}
	spans, err := Detect(frames, Config{AmplitudeThreshold: 0.01, MinDurationNS: 1, BytesPerFrame: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 0 {
		t.Fatal("a loud frame must not be reported as silence")
	}
}

func TestSilenceEndpointsUnifiedTimebase(t *testing.T) {
	start := clock.Time(100 * time.Millisecond)
	frames := []Frame{
		{Time: start, DurNS: int64(10 * time.Millisecond), Payload: []byte{0, 0, 0, 0}},
		{Time: clock.Time(110 * time.Millisecond), DurNS: int64(10 * time.Millisecond), Payload: []byte{0, 0, 0, 0}},
		{Time: clock.Time(120 * time.Millisecond), DurNS: int64(10 * time.Millisecond), Payload: []byte{0, 0, 0, 0x7f}},
	}
	spans, err := Detect(frames, Config{AmplitudeThreshold: 0.01, MinDurationNS: 1, BytesPerFrame: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 || spans[0].Range.Start != start ||
		spans[0].Range.End != clock.Time(120*time.Millisecond) {
		t.Fatalf("unified-time endpoints wrong: %+v", spans)
	}
}
