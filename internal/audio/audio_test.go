package audio

import (
	"errors"
	"math"
	"testing"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

func TestChannelOrders(t *testing.T) {
	cases := map[Layout][]string{
		LayoutMono:   {"C"},
		LayoutStereo: {"L", "R"},
		Layout51:     {"L", "R", "C", "LFE", "Ls", "Rs"},
		Layout71:     {"L", "R", "C", "LFE", "Ls", "Rs", "Lrs", "Rrs"},
	}
	for l, want := range cases {
		got, err := ChannelOrder(l)
		if err != nil || len(got) != len(want) {
			t.Fatalf("%s: %v %v", l, got, err)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s order %v", l, got)
			}
		}
	}
	if _, err := ChannelOrder("quad"); err == nil {
		t.Fatal("unknown layout must fail")
	}
	if l, err := LayoutForChannels(6); err != nil || l != Layout51 {
		t.Fatalf("6ch -> %v %v", l, err)
	}
	if _, err := LayoutForChannels(4); err == nil {
		t.Fatal("4ch is ambiguous")
	}
	if LayoutName("5.1film") != Layout51Film || LayoutName("2.0") != LayoutStereo {
		t.Fatal("alias normalization")
	}
}

func TestMapLayoutIdentity(t *testing.T) {
	in := []int32{1, 2, 3, 4, 5, 6}
	out, lay, err := MapLayout(1, in, Layout51, Layout51, nil)
	if err != nil || lay != Layout51 {
		t.Fatalf("%v %v", lay, err)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("identity changed sample %d", i)
		}
	}
}

func TestMapLayoutRequiresExplicitMapping(t *testing.T) {
	in := make([]int32, 6*10)
	_, _, err := MapLayout(3, in, Layout51, LayoutStereo, nil)
	var ie *IncompatibleError
	if !errors.As(err, &ie) || ie.TrackID != 3 || ie.From != Layout51 || ie.To != LayoutStereo {
		t.Fatalf("bad error %v", err)
	}
}

func TestMapLayoutDownmix(t *testing.T) {
	m := StereoFrom51()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	// One frame: L=100 R=200 C=300 Ls=400 Rs=500 LFE=999
	in := []int32{100, 200, 300, 999, 400, 500}
	out, lay, err := MapLayout(1, in, Layout51, LayoutStereo, &m)
	if err != nil || lay != LayoutStereo {
		t.Fatalf("%v %v", lay, err)
	}
	// L: 100 + 300/2 + 400/2 = 450; R: 200 + 300/2 + 500/2 = 600
	if out[0] != 450 || out[1] != 600 {
		t.Fatalf("downmix %v", out)
	}
}

func TestMapLayoutRejectsBadMapping(t *testing.T) {
	m := StereoFrom51()
	m.Ops[1].Out = 5 // outside stereo
	if err := m.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
	// A mapping that leaves the right channel unfed.
	m2 := Mapping{From: Layout51, To: LayoutStereo, Ops: []ChannelMap{{In: 0, Out: 0, Num: 1, Den: 1}}}
	if err := m2.Validate(); err == nil {
		t.Fatal("unfed output must fail")
	}
	// A mapping for the wrong layouts.
	if _, _, err := MapLayout(1, make([]int32, 12), Layout51, LayoutStereo, &m2); err == nil {
		t.Fatal("wrong-layout mapping must fail")
	}
}

func TestMapLayoutIndivisible(t *testing.T) {
	_, _, err := MapLayout(9, make([]int32, 7), Layout51, Layout51, nil)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeSampleIndivisible || fe.TrackID != 9 {
		t.Fatalf("bad error %v", err)
	}
}

func TestResampleExact(t *testing.T) {
	tb := clock.Must(48000, 1)
	in := make([]int32, 480*2)
	for i := 0; i < 480; i++ {
		in[i*2] = int32(i * 100)
		in[i*2+1] = int32(-i * 100)
	}
	out, err := Resample(1, in, 2, 48000, 96000, tb)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 960*2 {
		t.Fatalf("frames %d", len(out)/2)
	}
	if out[0] != 0 || out[2] != 50 {
		t.Fatalf("interpolation %v", out[:4])
	}
}

func TestResampleIndivisible(t *testing.T) {
	tb := clock.Must(48000, 1)
	// 1 frame at 48000 -> 44100 needs 0.91875 frames: rejected.
	_, err := Resample(4, []int32{1, 2}, 2, 48000, 44100, tb)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeSampleIndivisible {
		t.Fatalf("bad error %v", err)
	}
	if fe.TrackID != 4 || fe.Span == nil {
		t.Fatalf("missing location %+v", fe)
	}
	var re *ResampleError
	if !errors.As(err, &re) || re.InRate != 48000 || re.OutRate != 44100 {
		t.Fatalf("lost detail %v", err)
	}
	if !containsStr(fe.Error(), "48000 Hz") {
		t.Fatalf("message %q", fe.Error())
	}
}

func TestResampleSameRateCopies(t *testing.T) {
	tb := clock.Must(48000, 1)
	in := []int32{1, 2, 3, 4}
	out, err := Resample(1, in, 2, 48000, 48000, tb)
	if err != nil || len(out) != 4 || out[3] != 4 {
		t.Fatalf("%v %v", out, err)
	}
	out[0] = 99
	if in[0] != 1 {
		t.Fatal("must copy, not alias")
	}
}

func TestResampleInvalid(t *testing.T) {
	tb := clock.Must(48000, 1)
	if _, err := Resample(1, []int32{1}, 0, 48000, 48000, tb); err == nil {
		t.Fatal("zero channels")
	}
	if _, err := Resample(1, []int32{1}, 1, 0, 48000, tb); err == nil {
		t.Fatal("zero rate")
	}
	if _, err := Resample(1, []int32{1, 2, 3}, 2, 48000, 96000, tb); err == nil {
		t.Fatal("non-frame buffer")
	}
}

func TestApplyLoudnessHitsTarget(t *testing.T) {
	// A quiet full-scale-free sine must be boosted to the target.
	const rate = 48000
	in := make([]int32, rate)
	for i := range in {
		in[i] = int32(2000 * math.Sin(2*math.Pi*1000*float64(i)/rate))
	}
	target := DefaultTarget()
	out, stats, err := ApplyLoudness(1, in, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatal("length changed")
	}
	if !stats.Limited || stats.LimitReason != LimitGainBounds {
		t.Fatalf("quiet input should hit the gain bound: %+v", stats)
	}
	if math.Abs(stats.OutputLUFS-stats.InputLUFS-stats.AppliedGainDB) > 0.2 {
		t.Fatalf("gain not applied: in %.2f out %.2f gain %.2f", stats.InputLUFS, stats.OutputLUFS, stats.AppliedGainDB)
	}
}

func TestApplyLoudnessLimitsPeak(t *testing.T) {
	// A signal close to full scale at low loudness claims a large gain;
	// the peak ceiling must win and record why.
	in := make([]int32, 480)
	// One near-full-scale impulse in near silence: the loudness gain would
	// be large, so the peak ceiling must clamp it.
	in[0] = math.MaxInt32
	_, stats, err := ApplyLoudness(2, in, LoudnessTarget{TargetLUFS: -23, TruePeakDBTP: -1, MaxGainDB: 40, MinGainDB: -40})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Limited || stats.LimitReason != LimitPeakCeiling {
		t.Fatalf("peak ceiling not applied: %+v", stats)
	}
	if stats.Clipped != 0 {
		t.Fatalf("clipped %d", stats.Clipped)
	}
	if stats.OutputPeak > 1 {
		t.Fatalf("output peak %.3f", stats.OutputPeak)
	}
}

func TestApplyLoudnessSilence(t *testing.T) {
	_, stats, err := ApplyLoudness(3, make([]int32, 100), DefaultTarget())
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeLoudnessTarget {
		t.Fatalf("bad error %v", err)
	}
	if stats.LimitReason != LimitSilence {
		t.Fatalf("reason %q", stats.LimitReason)
	}
}

func TestApplyLoudnessDisabled(t *testing.T) {
	in := []int32{1, -2, 3}
	out, stats, err := ApplyLoudness(1, in, LoudnessTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if out[1] != -2 || stats.AppliedGainDB != 0 {
		t.Fatalf("%v %+v", out, stats)
	}
	if _, _, err := ApplyLoudness(1, nil, DefaultTarget()); err == nil {
		t.Fatal("empty buffer")
	}
}

func TestStatsValidReportsUnrecordedLimit(t *testing.T) {
	s := LoudnessStats{Limited: true}
	if s.StatsValid() == nil {
		t.Fatal("missing reason must be invalid")
	}
	s = LoudnessStats{TargetLUFS: -23, OutputLUFS: -23, Limited: true, LimitReason: LimitPeakCeiling}
	if err := s.StatsValid(); err != nil {
		t.Fatal(err)
	}
	s.OutputLUFS = -10
	if s.StatsValid() == nil {
		t.Fatal("far from target must be invalid")
	}
}

func TestStreamingLoudnessMatchesBatch(t *testing.T) {
	// Measuring in windows must equal measuring the whole buffer, otherwise
	// a streamed delivery would not be reproducible from its parts.
	const rate = 48000
	samples := make([]int32, rate)
	for i := range samples {
		samples[i] = int32(float64(1<<28) * math.Sin(2*math.Pi*440*float64(i)/rate))
	}
	batchLUFS, batchPeak := Measure(samples)
	acc := &Accumulator{}
	for start := 0; start < len(samples); start += 1000 {
		end := start + 1000
		if end > len(samples) {
			end = len(samples)
		}
		acc.Add(samples[start:end])
	}
	if diff := math.Abs(acc.LUFS() - batchLUFS); diff > 1e-9 {
		t.Fatalf("windowed LUFS %.9f vs batch %.9f", acc.LUFS(), batchLUFS)
	}
	if math.Abs(acc.Peak()-batchPeak) > 1e-12 {
		t.Fatalf("peak %.9f vs %.9f", acc.Peak(), batchPeak)
	}
	if acc.Count() != int64(len(samples)) {
		t.Fatalf("count %d", acc.Count())
	}
}

func TestPlanGainMatchesApplyLoudness(t *testing.T) {
	const rate = 48000
	in := make([]int32, rate)
	for i := range in {
		in[i] = int32(float64(1<<27) * math.Sin(2*math.Pi*1000*float64(i)/rate))
	}
	target := DefaultTarget()
	lufs, peak := Measure(in)
	plan, err := PlanGain(1, lufs, peak, target)
	if err != nil {
		t.Fatal(err)
	}
	_, batch, err := ApplyLoudness(1, in, target)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(plan.AppliedGainDB-batch.AppliedGainDB) > 1e-9 {
		t.Fatalf("plan %.6f vs batch %.6f", plan.AppliedGainDB, batch.AppliedGainDB)
	}
	if plan.Limited != batch.Limited || plan.LimitReason != batch.LimitReason {
		t.Fatalf("plan %+v vs batch %+v", plan, batch)
	}
	out, clipped := ApplyGain(in, plan.AppliedGainDB)
	if clipped != 0 {
		t.Fatalf("clipped %d", clipped)
	}
	if len(out) != len(in) {
		t.Fatal("length changed")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
