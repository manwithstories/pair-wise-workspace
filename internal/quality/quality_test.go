package quality

import (
	"encoding/binary"
	"math"
	"testing"

	"mediaflow/internal/clock"
)

func vfrFrames() []clock.Normalized {
	// VFR durations deliberately differ; a gap is inserted between frame 3
	// and frame 4 to trigger a discontinuity cut.
	durs := []int64{40_000, 36_000, 44_000, 40_000, 38_000, 42_000}
	var frames []clock.Normalized
	cur := int64(0)
	for i, d := range durs {
		start := cur
		if i == 4 {
			start = cur + 200_000 // discontinuity
		}
		frames = append(frames, clock.Normalized{
			Track: 1, Seq: uint32(i), StartUS: start,
			Duration: d, NativeDur: d * 90, Payload: []byte{byte(i)},
		})
		cur = start + d
	}
	return frames
}

func TestDetectCutsVFR(t *testing.T) {
	frames := vfrFrames()
	cuts, segments, err := DetectCuts(frames, DefaultCutConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(cuts) == 0 || cuts[0].Reason != "discontinuity" {
		t.Fatalf("expected discontinuity cut, got %+v", cuts)
	}
	if len(segments) < 2 {
		t.Fatalf("expected >=2 segments, got %d", len(segments))
	}
	// VFR frame durations must survive: total covered time equals sum of frame
	// durations (the discontinuity gap is excluded, segments stay frame-aligned).
	for _, s := range segments {
		if s.EndFrame <= s.StartFrame {
			t.Fatalf("empty segment: %+v", s)
		}
		var sum int64
		for i := s.StartFrame; i < s.EndFrame; i++ {
			sum += frames[i].Duration
		}
		if sum != s.EndUS-s.StartUS {
			t.Fatalf("VFR duration not preserved in segment: sum=%d span=%d", sum, s.EndUS-s.StartUS)
		}
	}
}

func TestHintSnapsToFrame(t *testing.T) {
	frames := vfrFrames()
	cfg := DefaultCutConfig()
	cfg.HintUS = []int64{frames[2].StartUS + 100} // near frame 2
	cuts, _, err := DetectCuts(frames, cfg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cuts {
		if c.Reason == "hint" && c.AtUS == frames[2].StartUS {
			found = true
		}
	}
	if !found {
		t.Fatalf("hint not snapped to frame start: %+v", cuts)
	}
}

func TestSilenceHysteresis(t *testing.T) {
	cfg := DefaultSilenceConfig()
	quiet := make([]float64, 64) // all zero -> RMS 0
	// Mid-level samples between enter and exit thresholds test hysteresis.
	mid := make([]float64, 64)
	midLevel := (cfg.EnterThreshold + cfg.ExitThreshold) / 2
	for i := range mid {
		mid[i] = midLevel
	}
	loud := make([]float64, 64)
	for i := range loud {
		loud[i] = 0.9
	}
	frames := []AudioFrame{
		{Frame: clock.Normalized{Track: 2, StartUS: 0, Duration: 100_000}, Samples: loud},
		{Frame: clock.Normalized{Track: 2, StartUS: 100_000, Duration: 100_000}, Samples: quiet},
		{Frame: clock.Normalized{Track: 2, StartUS: 200_000, Duration: 100_000}, Samples: mid},
		{Frame: clock.Normalized{Track: 2, StartUS: 300_000, Duration: 100_000}, Samples: loud},
	}
	ranges, err := DetectSilence(frames, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Quiet + mid-level stay merged into one silent range thanks to hysteresis.
	if len(ranges) != 1 {
		t.Fatalf("expected 1 merged silence range, got %d: %+v", len(ranges), ranges)
	}
	if ranges[0].StartUS != 100_000 || ranges[0].EndUS != 300_000 {
		t.Fatalf("silence bounds wrong: %+v", ranges[0])
	}
}

func TestSilenceMinDurationFilter(t *testing.T) {
	cfg := DefaultSilenceConfig()
	cfg.MinDurationUS = 500_000
	frames := []AudioFrame{
		{Frame: clock.Normalized{Track: 2, StartUS: 0, Duration: 100_000}, Samples: make([]float64, 32)},
	}
	ranges, err := DetectSilence(frames, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 0 {
		t.Fatalf("short dip must be filtered, got %+v", ranges)
	}
}

func TestSilenceInvalidHysteresis(t *testing.T) {
	cfg := SilenceConfig{EnterThreshold: 0.1, ExitThreshold: 0.05}
	if _, err := DetectSilence(nil, cfg); err == nil {
		t.Fatal("expected hysteresis validation error")
	}
}

func TestRMSKnownValue(t *testing.T) {
	got := RMS([]float64{3, 4})
	want := math.Sqrt((9 + 16) / 2.0)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("RMS got %v want %v", got, want)
	}
}

func TestFingerprintDuplicateTwoStage(t *testing.T) {
	pay := []byte{1, 2, 3, 4, 5}
	// A repeated content block: identical payload and identical timing shape.
	frames := []clock.Normalized{
		{Track: 1, Seq: 0, StartUS: 0, Duration: 40_000, Payload: pay},
		{Track: 1, Seq: 1, StartUS: 40_000, Duration: 40_000, Payload: []byte{9}},
		{Track: 1, Seq: 2, StartUS: 0, Duration: 40_000, Payload: pay}, // exact duplicate of frame 0
	}
	matches := FindDuplicates(frames, DefaultFingerprintConfig())
	if len(matches) != 1 || matches[0].FrameA != 0 || matches[0].FrameB != 2 {
		t.Fatalf("duplicate match wrong: %+v", matches)
	}
	if !matches[0].ByteEqual {
		t.Fatal("verified match must be byte-equal")
	}
}

func TestFingerprintCollisionNotDeleted(t *testing.T) {
	// Stage 1 only ever *nominates* candidates. Here two frames share timing
	// (identical timing-only hash input) but carry different payloads, so the
	// stage-2 byte comparison must reject the candidate and emit no match.
	frames := []clock.Normalized{
		{Track: 1, Seq: 0, StartUS: 0, Duration: 40_000, Payload: []byte{1}},
		{Track: 1, Seq: 1, StartUS: 0, Duration: 40_000, Payload: []byte{2}},
	}
	timingHash := func(f clock.Normalized) uint64 {
		// Timing-only hash: deliberately ignores payload, modeling the cheap
		// candidate-stage fingerprint that can collide across content.
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(f.StartUS)^uint64(f.Duration))
		return binary.LittleEndian.Uint64(buf[:])
	}
	if timingHash(frames[0]) != timingHash(frames[1]) {
		t.Fatal("test setup: timing hashes must collide")
	}
	// FindDuplicates always applies the full byte-equality stage regardless of
	// how the candidate was nominated, so differing payloads yield zero matches.
	matches := FindDuplicates(frames, DefaultFingerprintConfig())
	if len(matches) != 0 {
		t.Fatalf("payload mismatch must not be reported as duplicate: %+v", matches)
	}
}
