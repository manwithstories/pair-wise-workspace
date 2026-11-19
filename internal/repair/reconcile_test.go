package repair

import (
	"testing"

	"pairflow/internal/clock"
	"pairflow/internal/rtp"
	"pairflow/internal/testkit"
	"pairflow/internal/wav"
)

func block(device string, seq uint64, rawSeq uint16, ts uint32, idx int, f testkit.PCMFormat) *wav.Block {
	pcm := testkit.SynthesizePCM(f, idx, testkit.FramesPerBlock, 1)
	return wav.ParseBlock(device, int64(seq)*100, seq, rawSeq, ts, 0,
		testkit.BuildWAVBlock(f, pcm))
}

func TestRepairWrapAndGap(t *testing.T) {
	f := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	r := NewReconciler("d")
	// 构造 32bit 时间戳回绕：末段时间戳越过 0xFFFFFFFF。
	near := uint32(0xFFFFFFFF) - uint32(2*testkit.FramesPerBlock)
	var gaps []rtp.Gap
	for i := 0; i < 6; i++ {
		seq := uint64(i)
		ts := near + uint32(i*testkit.FramesPerBlock)
		blk := block("d", seq, uint16(i), ts, i, f)
		r.HandleBlock(blk, gaps)
	}
	rr := r.Result()
	if rr.Timeline == nil || rr.Timeline.WrapEvents < 1 {
		t.Fatalf("timestamp wrap must be recorded: %+v", rr.Timeline)
	}
	if rr.Timeline.TotalSamples() <= 0 {
		t.Fatal("timeline must contain samples after wrap")
	}
}

func TestRepairBurstGapNotCompressed(t *testing.T) {
	f := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	r := NewReconciler("d")
	// 序号 0,1 后直接跳到 4，中间 2,3 由 RTP 缺口声明（缺失 48 采样）。
	b0 := block("d", 0, 0, 0, 0, f)
	r.HandleBlock(b0, nil)
	b1 := block("d", 1, 1, testkit.FramesPerBlock, 1, f)
	r.HandleBlock(b1, nil)
	gaps := []rtp.Gap{{DeviceID: "d", StartSeq: 2, EndSeq: 4,
		MissingPackets: 2, EstSamples: int64(2 * testkit.FramesPerBlock)}}
	b4 := block("d", 4, 4, 4*testkit.FramesPerBlock, 4, f)
	rr := r.HandleBlock(b4, gaps)
	if rr.GapCount != 1 {
		t.Fatalf("want 1 gap, got %d", rr.GapCount)
	}
	// 时间线总长必须包含缺口（不压缩）：3 个真实块(72) + 48 缺失采样 = 120。
	want := int64(3*testkit.FramesPerBlock) + int64(2*testkit.FramesPerBlock)
	if got := rr.Timeline.TotalSamples(); got != want {
		t.Fatalf("timeline compressed: got %d want %d", got, want)
	}
	foundBurst := false
	for _, u := range rr.Uncertain {
		if u.Kind == clock.KindBurstLoss && u.Length() == int64(2*testkit.FramesPerBlock) {
			foundBurst = true
		}
	}
	if !foundBurst {
		t.Fatal("burst loss must be an explicit uncertain range")
	}
}

func TestRepairDriftUsesSampleCount(t *testing.T) {
	f := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	r := NewReconciler("d")
	r.HandleBlock(block("d", 0, 0, 0, 0, f), nil)
	// 第二块时间戳只前进 23 而非 24（漂移 1 采样），采样计数仍应主导放置。
	b := block("d", 1, 1, testkit.FramesPerBlock-1, 1, f)
	rr := r.HandleBlock(b, nil)
	if rr.Timeline.Repaired < 1 {
		t.Fatal("timestamp drift must be repaired using trusted sample count")
	}
	if rr.Timeline.NextSample() != 2*testkit.FramesPerBlock {
		t.Fatalf("sample count must drive placement, next=%d", rr.Timeline.NextSample())
	}
}

func TestIndeterminateWhenNoClockBaseline(t *testing.T) {
	f := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	r := NewReconciler("d")
	// 首个块即截断：时间线尚未建立、无采样率基准，长度不可判定。
	trunc := testkit.TruncateWAV(testkit.BuildWAVBlock(f, testkit.SynthesizePCM(f, 0, 8, 1)), 8)
	blk := wav.ParseBlock("d", 0, 0, 0, 0, 0, trunc)
	rr := r.HandleBlock(blk, nil)
	if !rr.HasIndeterminate() {
		t.Fatal("first-block parse failure with no clock baseline must be indeterminate")
	}
}

func TestTruncatedAfterBaselineIsKnownLoss(t *testing.T) {
	f := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	r := NewReconciler("d")
	r.HandleBlock(block("d", 0, 0, 0, 0, f), nil)
	// 时间线已建立后出现截断块：应归为已知突发丢失而非不可判定。
	trunc := testkit.TruncateWAV(testkit.BuildWAVBlock(f, testkit.SynthesizePCM(f, 1, testkit.FramesPerBlock, 1)), 8)
	blk := wav.ParseBlock("d", 1, 1, testkit.FramesPerBlock, 1, 0, trunc)
	rr := r.HandleBlock(blk, nil)
	if rr.HasIndeterminate() {
		t.Fatal("truncated block after clock baseline must be a known loss, not indeterminate")
	}
}
