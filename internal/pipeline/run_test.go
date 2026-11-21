package pipeline_test

import (
	"testing"

	"mediadeliver/internal/fixture"
	"mediadeliver/internal/pipeline"
)

func cfg() pipeline.Config {
	return pipeline.Config{SampleRate: 48000, Channels: 2, TargetLUFS: -16,
		PeakCeiling: -1, RTPWindow: 32, RTPClockRate: 48000}
}

func job(set fixture.Set, manifest []byte, rtp [][]byte) pipeline.Job {
	return pipeline.Job{ManifestName: "live.m3u8", ManifestData: manifest,
		Segments: set.Segments, RTPPackets: rtp}
}

func TestNormalPath(t *testing.T) {
	set := fixture.Generate(42, 4, 2.0)
	res := pipeline.Run(cfg(), job(set, set.ManifestGood, set.RTPGood), nil)
	if len(res.Faults) != 0 {
		t.Fatalf("faults: %v", res.Faults)
	}
	if res.SyncErrorMs > 5 {
		t.Fatalf("sync: %v", res.SyncErrorMs)
	}
	if res.RTPStats.RecoveryRate() < 0.80 {
		t.Fatalf("recovery: %v", res.RTPStats)
	}
	if res.Output.NumFrames() != 4*2*48000 {
		t.Fatalf("frames: %d", res.Output.NumFrames())
	}
}

func TestCorruptPath(t *testing.T) {
	set := fixture.Generate(42, 4, 2.0)
	j := job(set, set.ManifestGood, set.RTPGood)
	j.Segments[set.CorruptIndex].Fragment = set.Corrupt
	res := pipeline.Run(cfg(), j, nil)
	if res.Faults.FirstOffset() < 0 {
		t.Fatalf("need non-negative offset: %v", res.Faults)
	}
	// 损坏不阻断后续分片
	if len(res.Segments) != 4 {
		t.Fatalf("segments: %d", len(res.Segments))
	}
}

func TestRecoverPath(t *testing.T) {
	set := fixture.Generate(42, 4, 2.0)
	j := job(set, set.ManifestTrunc, set.RTPLossy)
	j.Segments[set.ShortTailIdx].Audio = set.ShortTailAudio
	res := pipeline.Run(cfg(), j, nil)
	// 截断清单保留前段
	if len(res.Playlist.Segments) != 3 || !res.Playlist.Truncated {
		t.Fatalf("playlist: %+v", res.Playlist)
	}
	// 非整帧 PCM 记边界错误且保留前段结果
	if res.Segments[set.ShortTailIdx].Samples != 2*48000 {
		t.Fatalf("short tail: %d", res.Segments[set.ShortTailIdx].Samples)
	}
	// 两类故障贯穿解析与转换阶段
	var haveManifest, haveConvert bool
	for _, f := range res.Faults {
		if f.Stage == "manifest" {
			haveManifest = true
		}
		if f.Stage == "convert" {
			haveConvert = true
		}
	}
	if !haveManifest || !haveConvert {
		t.Fatalf("faults: %v", res.Faults)
	}
	if res.RTPStats.RecoveryRate() < 0.80 {
		t.Fatalf("recovery: %+v", res.RTPStats)
	}
	// 重复运行摘要一致，前段结果可复用
	res2 := pipeline.Run(cfg(), j, res)
	if res.OutputDigest() != res2.OutputDigest() {
		t.Fatal("digest unstable across runs")
	}
	if !res2.Segments[0].Reused {
		t.Fatal("prefix segment should be reused")
	}
	// 时间轴连续：输出采样数等于各分片之和
	sum := 0
	for _, s := range res.Segments {
		sum += s.Samples
	}
	if sum != res.Output.NumFrames() {
		t.Fatalf("timeline gap: %d != %d", sum, res.Output.NumFrames())
	}
}

func TestInputChangeRejectsReuse(t *testing.T) {
	set := fixture.Generate(42, 4, 2.0)
	j := job(set, set.ManifestGood, set.RTPGood)
	res1 := pipeline.Run(cfg(), j, nil)
	j.Segments[0].Fragment = fixture.MediaSegment(999, 2, false)
	res2 := pipeline.Run(cfg(), j, res1)
	if res2.Segments[0].Reused {
		t.Fatal("changed input must not reuse old stage")
	}
	if res2.Segments[0].InputSHA == res1.Segments[0].InputSHA {
		t.Fatal("input digest must change")
	}
}
