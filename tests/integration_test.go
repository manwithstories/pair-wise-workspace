// Package integration_test 验证跨模块行为：夹具生成 → 解析 → 修复 → 索引 → 报告，
// 覆盖确定性、背压、续跑、损坏定位与取消。
package integration_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"mediarepair/internal/fault"
	"mediarepair/internal/fixture"
	"mediarepair/internal/pipeline"
	"mediarepair/internal/repair"
)

func generate(t *testing.T, pattern fixture.Pattern) fixture.Files {
	t.Helper()
	files, err := fixture.Generate(t.TempDir(), fixture.Spec{
		VideoSamples: 120, AudioSamples: 60, VideoTimescale: 90_000, AudioTimescale: 48_000, Pattern: pattern,
	})
	if err != nil {
		t.Fatalf("生成夹具: %v", err)
	}
	return files
}

func run(t *testing.T, opts pipeline.Options) *pipeline.Outcome {
	t.Helper()
	if opts.Strategy == (repair.Strategy{}) {
		opts.Strategy = repair.DefaultStrategy()
	}
	out, err := pipeline.Run(opts)
	if err != nil {
		t.Fatalf("流水线失败: %v", err)
	}
	return out
}

func TestAcceptanceThresholds(t *testing.T) {
	files := generate(t, fixture.PatternClean)
	out := run(t, pipeline.Options{Input: files.Timeline, Buffer: 8, SlowConsumer: 1})
	r := out.Result
	if r.RoundtripMatch < 0.99 {
		t.Fatalf("roundtrip_match=%v", r.RoundtripMatch)
	}
	if r.KeyframeHitRate < 0.98 {
		t.Fatalf("keyframe_hit_rate=%v", r.KeyframeHitRate)
	}
	if len(r.SegmentHashes) < 3 {
		t.Fatalf("segment_hashes=%d", len(r.SegmentHashes))
	}
	if r.PeakBufferSamples > 8 {
		t.Fatalf("peak_buffer_samples=%d > 8", r.PeakBufferSamples)
	}
	if r.BackpressureWait <= 0 {
		t.Fatalf("backpressure_wait_ms=%d 应 > 0", r.BackpressureWait)
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	files := generate(t, fixture.PatternClean)
	a := run(t, pipeline.Options{Input: files.Timeline, Buffer: 4})
	b := run(t, pipeline.Options{Input: files.Timeline, Buffer: 4})
	if len(a.Result.SegmentHashes) != len(b.Result.SegmentHashes) {
		t.Fatal("两次运行片段哈希数量不同")
	}
	for i := range a.Result.SegmentHashes {
		if a.Result.SegmentHashes[i].Digest != b.Result.SegmentHashes[i].Digest {
			t.Fatalf("片段 %s 摘要不确定", a.Result.SegmentHashes[i].ID)
		}
	}
}

func TestEachPatternDetectsExpectedAnomaly(t *testing.T) {
	cases := []struct {
		pattern fixture.Pattern
		check   func(*pipeline.Outcome) bool
		name    string
	}{
		{fixture.PatternWrap, func(o *pipeline.Outcome) bool { return o.RTP.Wraps > 0 && videoWraps(o) > 0 }, "回绕"},
		{fixture.PatternDuplicate, func(o *pipeline.Outcome) bool { return o.RTP.Duplicates > 0 }, "重复"},
		{fixture.PatternRegression, func(o *pipeline.Outcome) bool { return o.RTP.Regressions > 0 }, "倒退"},
		{fixture.PatternClipJump, func(o *pipeline.Outcome) bool { return videoJumps(o) > 0 }, "跨剪辑跳变"},
	}
	for _, c := range cases {
		files := generate(t, c.pattern)
		out := run(t, pipeline.Options{Input: files.Timeline, Buffer: 4})
		if !c.check(out) {
			t.Fatalf("模式 %s 未检出%s: rtp=%+v tracks=%+v", c.pattern, c.name, out.RTP, out.Result.Tracks)
		}
	}
}

func videoWraps(o *pipeline.Outcome) uint64 {
	for _, tr := range o.Result.Tracks {
		if tr.TrackID == 1 {
			return tr.Wraps
		}
	}
	return 0
}

func videoJumps(o *pipeline.Outcome) uint64 {
	for _, tr := range o.Result.Tracks {
		if tr.TrackID == 1 {
			return tr.ClipJumps
		}
	}
	return 0
}

func TestResumeReusesVerifiedHashes(t *testing.T) {
	files := generate(t, fixture.PatternClean)
	full := run(t, pipeline.Options{Input: files.Timeline, Buffer: 4})
	resumed := run(t, pipeline.Options{Input: files.Timeline, Buffer: 4, ResumeFrom: "v-trans-12"})

	fullHashes := map[string]string{}
	for _, h := range full.Result.SegmentHashes {
		fullHashes[h.ID] = h.Digest
	}
	if len(resumed.Result.SegmentHashes) >= len(fullHashes) {
		t.Fatalf("续跑不应重算全部片段: %d vs %d", len(resumed.Result.SegmentHashes), len(fullHashes))
	}
	for _, h := range resumed.Result.SegmentHashes {
		if fullHashes[h.ID] != h.Digest {
			t.Fatalf("续跑片段 %s 摘要与全量运行不一致", h.ID)
		}
	}
}

func TestCorruptInputReportsOffsetAndStage(t *testing.T) {
	files := generate(t, fixture.PatternClean)
	_, err := pipeline.Run(pipeline.Options{Input: files.Corrupt, Buffer: 4, Strategy: repair.DefaultStrategy()})
	if err == nil {
		t.Fatal("截断输入应失败")
	}
	fe, ok := fault.As(err)
	if !ok {
		t.Fatalf("应为 fault.Error: %v", err)
	}
	if fe.Stage != fault.StageParse || fe.Offset <= 0 {
		t.Fatalf("定位信息不足: %+v", fe)
	}
}

// cancelWriter 在累计写入若干样本后取消上下文，模拟中断。
type cancelWriter struct {
	cancel context.CancelFunc
	after  int
	n      int
}

func (c *cancelWriter) Write(p []byte) (int, error) {
	c.n++
	if c.n == c.after {
		c.cancel()
	}
	return len(p), nil
}

func TestCancellationStopsPromptly(t *testing.T) {
	files := generate(t, fixture.PatternClean)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelWriter{cancel: cancel, after: 20}
	start := time.Now()
	out, err := pipeline.Run(pipeline.Options{Input: files.Timeline, Buffer: 2, Context: ctx,
		Strategy: repair.DefaultStrategy(), SampleSink: w})
	if err == nil {
		t.Fatal("取消后应返回错误")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("取消未及时生效")
	}
	if out == nil || out.Result == nil {
		t.Fatal("中断时应保留已验证片段的报告")
	}
}

func TestSampleSinkIsStable(t *testing.T) {
	files := generate(t, fixture.PatternClean)
	var a, b bytes.Buffer
	run(t, pipeline.Options{Input: files.Timeline, Buffer: 4, SampleSink: &a})
	run(t, pipeline.Options{Input: files.Timeline, Buffer: 4, SampleSink: &b})
	if a.Len() == 0 || a.Len() != b.Len() || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("修复后样本流编码不确定")
	}
}

func TestBufferCapacityBounded(t *testing.T) {
	files := generate(t, fixture.PatternClean)
	out := run(t, pipeline.Options{Input: files.Timeline, Buffer: 3, SlowConsumer: 1})
	if out.Result.PeakBufferSamples > 3 {
		t.Fatalf("峰值 %d 超过容量 3", out.Result.PeakBufferSamples)
	}
	if out.Result.BufferCapacity != 3 {
		t.Fatalf("容量记录 %d", out.Result.BufferCapacity)
	}
}

func TestUnknownTimelineReferenceFails(t *testing.T) {
	files := generate(t, fixture.PatternClean)
	// 指向不存在的 sidecar。
	_, err := pipeline.Run(pipeline.Options{
		Input: files.Timeline, TimelinePath: filepath.Join(t.TempDir(), "missing.json"),
		Buffer: 4, Strategy: repair.DefaultStrategy(),
	})
	if err == nil {
		t.Fatal("缺少时间线应失败")
	}
}
