package verify

import (
	"math"
	"testing"

	"archivecheck/internal/clock"
	"archivecheck/internal/timeline"
)

func TestPSNRExactMatch(t *testing.T) {
	samples := []Sample{{Ref: 100, Got: 100}, {Ref: -200, Got: -200}}
	st := PSNR(samples)
	if st.Samples != 2 || st.MSE != 0 || st.PSNRdB != 99 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestPSNRErrors(t *testing.T) {
	// Constant error of 1 on 16-bit samples: MSE=1, PSNR=20*log10(32767) ~ 90.3 dB.
	samples := make([]Sample, 100)
	for i := range samples {
		samples[i] = Sample{Ref: 0, Got: 1}
	}
	st := PSNR(samples)
	if math.Abs(st.PSNRdB-90.3080) > 0.01 {
		t.Fatalf("psnr=%v", st.PSNRdB)
	}
	// Bigger errors reduce PSNR.
	for i := range samples {
		samples[i].Got = 100
	}
	if PSNR(samples).PSNRdB >= st.PSNRdB {
		t.Fatal("larger error should reduce PSNR")
	}
}

func TestPSNREmptyAndMissing(t *testing.T) {
	st := PSNR(nil)
	if st.Samples != 0 || st.PSNRdB != 0 {
		t.Fatalf("empty=%+v", st)
	}
	st = PSNR([]Sample{{Missing: true}, {Ref: 1, Got: 1}})
	if st.Samples != 1 || st.Missing != 1 || st.PSNRdB != 99 {
		t.Fatalf("missing=%+v", st)
	}
}

func TestPSNRWeighted(t *testing.T) {
	// One error of 2 with weight 3 -> MSE 4; one exact with weight 1.
	st := PSNR([]Sample{{Ref: 0, Got: 2, Weight: 3}, {Ref: 0, Got: 0, Weight: 1}})
	wantMSE := 3.0 * 4 / 4.0
	if math.Abs(st.MSE-wantMSE) > 1e-9 || st.Samples != 4 {
		t.Fatalf("weighted=%+v", st)
	}
}

func TestBitrateWithinTarget(t *testing.T) {
	// 2 Mbps over 4s -> 1 MB exactly -> 2 Mbps actual.
	b := ComputeBitrate(2_000_000, 1_000_000, 4.0)
	if b.ActualBps != 2_000_000 || b.DeltaPct != 0 || !b.WithinTarget {
		t.Fatalf("bitrate=%+v", b)
	}
	// 4% under target is within 5%.
	b = ComputeBitrate(2_000_000, 960_000, 4.0)
	if math.Abs(b.DeltaPct+4) > 0.01 || !b.WithinTarget {
		t.Fatalf("4%% under: %+v", b)
	}
	// 6% over is outside the tolerance.
	b = ComputeBitrate(2_000_000, 1_060_000, 4.0)
	if b.WithinTarget {
		t.Fatalf("6%% over should fail: %+v", b)
	}
}

func TestBitrateEdgeCases(t *testing.T) {
	if b := ComputeBitrate(1_000_000, 500, 0); b.ActualBps != 0 || b.WithinTarget {
		t.Fatalf("zero duration=%+v", b)
	}
	if b := ComputeBitrate(0, 500, 1); b.DeltaPct != 0 {
		t.Fatalf("no target=%+v", b)
	}
}

func TestTrackBitrates(t *testing.T) {
	info := &timeline.TrackInfo{Number: 1}
	info.Frames = []timeline.Frame{
		{Track: 1, Offset: 0, Repaired: clock.MustNew(0, 1), Bytes: 1000},
		{Track: 1, Offset: 1, Repaired: clock.MustNew(2, 1), Bytes: 1000},
	}
	out := Tracks(map[uint64]*timeline.TrackInfo{1: info}, []uint64{1})
	if len(out) != 1 || out[0].Bytes != 2000 || out[0].Frames != 2 {
		t.Fatalf("tracks=%+v", out)
	}
	// 16000 bits over 2s = 8000 bps.
	if out[0].Bps != 8000 {
		t.Fatalf("bps=%d", out[0].Bps)
	}
}

func TestDeterministicSignature(t *testing.T) {
	a := Stats{Quality: QualityStats{PSNRdB: 88, Samples: 10}, IndexEntries: 3, IndexSHA256: "abc", ManifestAlignMS: 0.5}
	b := a
	b.Quality.Samples = 11
	if Deterministic(a, b) {
		t.Fatal("different sample counts must differ")
	}
	c := a
	c.Quality.MSE = 0.0001
	if Deterministic(a, c) {
		t.Fatal("different MSE must differ")
	}
	if !Deterministic(a, a) {
		t.Fatal("identical stats must match")
	}
}
