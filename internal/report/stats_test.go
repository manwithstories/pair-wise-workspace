package report

import (
	"math"
	"testing"

	"pairflow/internal/clock"
	"pairflow/internal/index"
	"pairflow/internal/repair"
)

// makeResult 构造 n 帧、立体声 16bit、48kHz 的合成结果。
func makeResult(n int, uncertain ...clock.UncertainRange) *repair.Result {
	tl := clock.NewTimeline("d", 48000)
	tl.SeedRange(0, int64(n))
	rr := &repair.Result{DeviceID: "d", Timeline: tl}
	pcm := make([]byte, n*2*2) // 2 声道 * 2 字节
	for i := 0; i < n*2; i++ {
		// 交替削波与静音，使指标非退化。
		v := uint16(0)
		if i%4 == 0 {
			v = 0x7fff
		} else if i%4 == 1 {
			v = 0x8000
		}
		pcm[2*i] = byte(v)
		pcm[2*i+1] = byte(v >> 8)
	}
	rr.Segments = []repair.Segment{{
		StartSample: 0, SampleCount: int64(n), Channels: 2,
		SampleRate: 48000, Data: pcm,
	}}
	rr.Uncertain = uncertain
	for _, u := range uncertain {
		tl.Uncertain = append(tl.Uncertain, u)
	}
	return rr
}

func TestComputeBitrateMatchesTarget(t *testing.T) {
	rr := makeResult(960)
	entries := []index.Entry{{TrackID: 1, StartSample: 0, SampleCount: 960,
		DataOffset: 16, DataLength: int64(960 * 4)}}
	m := Compute(rr, entries, Options{TargetBitrateBps: 192000})
	if m.ActualBitrateBps != 192000 {
		t.Fatalf("actual bitrate = %d, want 192000", m.ActualBitrateBps)
	}
	if math.Abs(m.BitrateDeviationPct) > 5 {
		t.Fatalf("deviation %f exceeds ±5%%", m.BitrateDeviationPct)
	}
	if m.ClippedPct <= 0 || m.SilentPct <= 0 {
		t.Fatalf("clipped/silent must be >0: %+v", m)
	}
}

func TestMissingSamplePercentage(t *testing.T) {
	// 960 总采样中 96 不确定（时间线范围必须覆盖缺口）。
	rr := makeResult(864, clock.UncertainRange{
		StartSample: 864, EndSample: 960, Kind: clock.KindBurstLoss})
	rr.Timeline.SeedRange(0, 960)
	entries := []index.Entry{{TrackID: 1, StartSample: 0, SampleCount: 864}}
	m := Compute(rr, entries, Options{TargetBitrateBps: 192000})
	if math.Abs(m.MissingSamplePct-10.0) > 0.01 {
		t.Fatalf("missing pct = %f, want 10.0", m.MissingSamplePct)
	}
	if m.QualityP50 < 0 || m.QualityP50 > 100 || m.QualityP95 < m.QualityP50 {
		t.Fatalf("quality percentiles invalid: p50=%f p95=%f", m.QualityP50, m.QualityP95)
	}
}

func TestManifestAlignmentZeroWhenMatching(t *testing.T) {
	rr := makeResult(100)
	entries := []index.Entry{{TrackID: 1, StartSample: 0, SampleCount: 100}}
	m := Compute(rr, entries, Options{TargetBitrateBps: 192000})
	if m.ManifestAlignmentMS != 0 {
		t.Fatalf("fresh metric should have zero alignment, got %f", m.ManifestAlignmentMS)
	}
}
