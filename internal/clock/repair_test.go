package clock

import (
	"errors"
	"math"
	"testing"
)

func TestUnwrap33MonotoneAcrossWrap(t *testing.T) {
	// 在 2^33 边界附近构造一次回绕。
	raw := []uint64{WrapMod - 90_000, WrapMod - 45_000, 45_000, 90_000}
	got, wraps, err := Unwrap33(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wraps != 1 {
		t.Fatalf("wrap count = %d, want 1", wraps)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("not monotone at %d: %v", i, got)
		}
	}
	// 回绕后跨度 = 45000（到边界）+ 45000（从 0 起）= 90000 Tick。
	if d := got[2] - got[1]; d != 90_000 {
		t.Fatalf("post-wrap delta = %d, want 90000", d)
	}
}

func TestUnwrap33RejectsOverflow(t *testing.T) {
	if _, _, err := Unwrap33([]uint64{0, WrapMod}); !errors.Is(err, ErrWrapOverflow) {
		t.Fatalf("want ErrWrapOverflow, got %v", err)
	}
}

func TestUnwrap33Empty(t *testing.T) {
	got, wraps, err := Unwrap33(nil)
	if err != nil || wraps != 0 || len(got) != 0 {
		t.Fatalf("empty input misbehaved: %v %d %v", got, wraps, err)
	}
}

func TestTickConversionsRoundTrip(t *testing.T) {
	for _, sec := range []float64{0, 0.5, 1, 2.5, 10} {
		tk := SecondsToTick(sec)
		if math.Abs(TickToSeconds(tk)-sec) > 1e-9 {
			t.Fatalf("round trip failed for %v: %v", sec, TickToSeconds(tk))
		}
	}
	if TickRate != 90_000 || WrapMod != 1<<33 {
		t.Fatal("constant mismatch")
	}
}

// makeObs 构造带固定 ppm 漂移的观测序列，每秒一个片段、每片 90000 Tick。
func makeObs(n int, ppm float64) []Observation {
	obs := make([]Observation, n)
	for i := range obs {
		exp := Tick(i * 90_000)
		obs[i] = Observation{
			Sequence: i,
			Expected: exp,
			Observed: Tick(math.Round(float64(exp) * (1 + ppm/1e6))),
		}
	}
	return obs
}

func TestFitDriftRecoversKnownSlope(t *testing.T) {
	obs := makeObs(24, 200)
	m := FitDrift(obs, 8)
	if math.Abs(m.DriftPPM-200) > 1 {
		t.Fatalf("drift = %.2f ppm, want ~200", m.DriftPPM)
	}
	if m.Points != 8 {
		t.Fatalf("window points = %d, want 8", m.Points)
	}
}

func TestFitDriftNeutralOnShortInput(t *testing.T) {
	m := FitDrift([]Observation{{Sequence: 0, Expected: 0, Observed: 10}}, 4)
	if m.Slope != 1 || m.Intercept != 0 {
		t.Fatalf("short input must yield neutral model, got %+v", m)
	}
}

func TestRepairSamplesWrapDriftVFR(t *testing.T) {
	// 12 个片段，每片 2 个视频样本；时钟带 200ppm 漂移，并在第 7 片跨越 33 位回绕；
	// 第 5 片注入一个重复 DTS（VFR 重复时间戳）。
	const seqN = 12
	var in []Sample
	// 起点贴近 2^33 边界，使第 6 片附近恰好发生一次回绕。
	base := Tick(WrapMod) - 6*90_000
	expected := map[int]Tick{}
	for seq := 0; seq < seqN; seq++ {
		expected[seq] = base + Tick(seq)*90_000
		// 设备时钟带 200ppm 漂移：观测位置 = 标称位置 * 1.0002（自起点累计）。
		for k := 0; k < 2; k++ {
			nominal := base + Tick(seq)*90_000 + Tick(k)*45_000
			drifted := Tick(math.Round(float64(nominal-base) * 1.0002))
			raw := uint64((base + drifted) % Tick(WrapMod))
			if seq == 5 && k == 1 {
				// 强制与同片上一样本重复（VFR 重复时间戳）。
				prev := in[len(in)-1]
				raw = prev.RawDTS
			}
			in = append(in, Sample{
				Sequence: seq, TrackID: 1, SampleNumber: uint32(k + 1),
				RawDTS: raw, RawPTS: raw, IsVideo: true, IsSync: k == 0,
			})
		}
	}
	tl, err := RepairSamples(in, RepairConfig{Window: 6, OutlierTick: 9_000, Expected: expected}, nil)
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if tl.WrapCount != 1 {
		t.Fatalf("wrap count = %d, want 1", tl.WrapCount)
	}
	if math.Abs(tl.Drift.DriftPPM-200) > 50 {
		t.Fatalf("drift = %.1f ppm, want 150..250", tl.Drift.DriftPPM)
	}
	if tl.Duplicate != 1 {
		t.Fatalf("duplicate count = %d, want 1", tl.Duplicate)
	}
	// 修复后 DTS 必须严格单调，PTS-DTS 间隔（这里为 0）保持不变。
	for i := 1; i < len(tl.Samples); i++ {
		if tl.Samples[i].DTS <= tl.Samples[i-1].DTS {
			t.Fatalf("DTS not strictly monotone at %d", i)
		}
		if tl.Samples[i].PTS-tl.Samples[i].DTS !=
			Tick(int64(tl.Samples[i].RawPTS)-int64(tl.Samples[i].RawDTS)) {
			t.Fatalf("PTS/DTS interval not preserved at %d", i)
		}
	}
}

func TestRepairSamplesBackwardsAndGap(t *testing.T) {
	expected := map[int]Tick{0: 0, 1: 90_000, 2: 180_000, 3: 270_000}
	in := []Sample{
		{Sequence: 0, TrackID: 1, SampleNumber: 1, RawDTS: 100, RawPTS: 200, IsVideo: true},
		{Sequence: 1, TrackID: 1, SampleNumber: 1, RawDTS: 90, RawPTS: 190, IsVideo: true},
		{Sequence: 2, TrackID: 1, SampleNumber: 1, RawDTS: 180_100, RawPTS: 180_200, IsVideo: true},
		{Sequence: 3, TrackID: 1, SampleNumber: 1, RawDTS: 270_100, RawPTS: 270_200, IsVideo: true},
	}
	tl, err := RepairSamples(in, RepairConfig{Window: 4, OutlierTick: 20_000, Expected: expected}, []int{2})
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if tl.Backwards != 1 {
		t.Fatalf("backwards = %d, want 1", tl.Backwards)
	}
	var missingGap *Gap
	for i := range tl.Gaps {
		if tl.Gaps[i].Sequence == 2 && tl.Gaps[i].Missing {
			missingGap = &tl.Gaps[i]
		}
	}
	if missingGap == nil {
		t.Fatalf("missing-segment gap not materialized: %+v", tl.Gaps)
	}
	if missingGap.Start != 180_000 || missingGap.End != 270_000 {
		t.Fatalf("gap bounds wrong: %+v", missingGap)
	}
}

func TestRepairSamplesMarksUndecidable(t *testing.T) {
	// 片段 3 的观测远离线性模型，应被整段标记，且不得计入成功样本。
	expected := map[int]Tick{}
	var in []Sample
	for seq := 0; seq < 6; seq++ {
		expected[seq] = Tick(seq) * 90_000
		raw := uint64(seq * 90_000)
		if seq == 3 {
			raw = uint64(seq*90_000 + 50_000) // 远超 OutlierTick
		}
		in = append(in, Sample{
			Sequence: seq, TrackID: 1, SampleNumber: 1,
			RawDTS: raw, RawPTS: raw, IsVideo: true,
		})
	}
	tl, err := RepairSamples(in, RepairConfig{Window: 6, OutlierTick: 4_500, Expected: expected}, nil)
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if tl.Undecidable != 1 {
		t.Fatalf("undecidable = %d, want 1", tl.Undecidable)
	}
	if tl.ValidSampleCount() != len(in)-1 {
		t.Fatal("undecidable sample must not count as valid")
	}
	if !tl.Samples[3].IsUndecidable {
		t.Fatal("outlier segment not flagged on sample")
	}
}

func TestRepairSamplesEmptyRejected(t *testing.T) {
	if _, err := RepairSamples(nil, RepairConfig{}, nil); !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("want ErrEmptyInput, got %v", err)
	}
}
