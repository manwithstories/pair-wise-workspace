package metrics

import (
	"bytes"
	"strings"
	"testing"

	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/pipeline"
)

func summaryStub() *pipeline.Result {
	return &pipeline.Result{
		Segments: []pipeline.SegmentResult{
			{Index: 0, SHA256: strings.Repeat("a", 64), RoundTrip: 1, VideoSamples: 4, KeyframeHitRate: 1},
			{Index: 1, SHA256: strings.Repeat("b", 64), RoundTrip: 1, VideoSamples: 4, KeyframeHitRate: 1},
		},
		Counters:        pipeline.Counters{Parsed: 98, Dropped: 2, Late: 1, PartialFailures: 0},
		PeakBufferBytes: 512,
	}
}

func TestEvaluatePassesAndRatios(t *testing.T) {
	s := Evaluate(summaryStub(), 100, DefaultThresholds())
	if !s.Passed {
		t.Fatalf("expected pass, failures=%v", s.Failures)
	}
	if s.RoundTripConsistency != 0.98 {
		t.Fatalf("roundtrip=%f want 0.98", s.RoundTripConsistency)
	}
	if s.KeyframeHitRate != 1.0 || s.SegmentCount != 2 {
		t.Fatalf("hit/segments wrong: %f %d", s.KeyframeHitRate, s.SegmentCount)
	}
	if s.DroppedPackets != 2 || s.LatePackets != 1 {
		t.Fatalf("counters wrong: %+v", s)
	}
}

func TestEvaluateFailsOnThreshold(t *testing.T) {
	th := DefaultThresholds()
	th.MaxBufferBytes = 100
	s := Evaluate(summaryStub(), 100, th)
	if s.Passed || len(s.Failures) != 1 {
		t.Fatalf("buffer breach must fail: %+v", s)
	}
	th2 := DefaultThresholds()
	th2.MinRoundTrip = 0.99
	if s2 := Evaluate(summaryStub(), 100, th2); s2.Passed {
		t.Fatal("roundtrip below 0.99 must fail")
	}
}

func TestWriteTextFields(t *testing.T) {
	s := Evaluate(summaryStub(), 100, DefaultThresholds())
	var buf bytes.Buffer
	WriteText(&buf, s)
	out := buf.String()
	for _, key := range []string{
		"roundtrip_consistency=", "keyframe_hit_rate=", "segments=2",
		"segment_sha256=", "peak_buffer_bytes=", "dropped_packets=",
		"late_packets=", "partial_failures=", "passed=true",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("output missing %q:\n%s", key, out)
		}
	}
}

func TestNoVideoVacuousHitRate(t *testing.T) {
	res := &pipeline.Result{
		Segments: []pipeline.SegmentResult{{Index: 0, SHA256: "x", VideoSamples: 0}},
		Counters: pipeline.Counters{Parsed: 10},
	}
	s := Evaluate(res, 10, DefaultThresholds())
	if s.KeyframeHitRate != 1.0 || !s.Passed {
		t.Fatalf("audio-only run must vacuously pass: %+v", s)
	}
}

func TestErrorKindStable(t *testing.T) {
	// Guard the sentinel kind strings used by the CLI exit-code mapping.
	if mediaerr.KindTimeout != "consumer_timeout" || mediaerr.KindCanceled != "canceled" {
		t.Fatal("error kind constants changed; CLI exit mapping depends on them")
	}
}
