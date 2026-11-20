package parse_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediarepair/internal/fault"
	"mediarepair/internal/fixture"
	"mediarepair/internal/model"
	"mediarepair/internal/parse"
)

func genFixtures(t *testing.T, spec fixture.Spec) fixture.Files {
	t.Helper()
	dir := t.TempDir()
	files, err := fixture.Generate(dir, spec)
	if err != nil {
		t.Fatalf("生成夹具: %v", err)
	}
	return files
}

func TestOpenMP4TracksAndSamples(t *testing.T) {
	files := genFixtures(t, fixture.DefaultSpec())
	mf, err := parse.OpenMP4(files.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Close()
	if got := len(mf.Tracks()); got != 2 {
		t.Fatalf("轨道数 %d", got)
	}
	if mf.TotalSamples() != 180 {
		t.Fatalf("样本数 %d", mf.TotalSamples())
	}
	if n := mf.TrackSampleCount(1); n != 120 {
		t.Fatalf("视频样本 %d", n)
	}
	if n := mf.TrackSampleCount(2); n != 60 {
		t.Fatalf("音频样本 %d", n)
	}
}

func TestStreamSamplesReadsPayload(t *testing.T) {
	files := genFixtures(t, fixture.DefaultSpec())
	mf, err := parse.OpenMP4(files.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Close()
	var n int
	var lastDTS uint64
	err = mf.StreamSamples(context.Background(), func(s model.Sample) error {
		if len(s.Data) == 0 {
			return errEmpty
		}
		if s.DTS < lastDTS {
			return errOrder
		}
		lastDTS = s.DTS
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 180 {
		t.Fatalf("流式读取样本数 %d", n)
	}
}

var (
	errEmpty = &streamErr{"样本负载为空"}
	errOrder = &streamErr{"样本未按 DTS 归并"}
)

type streamErr struct{ msg string }

func (e *streamErr) Error() string { return e.msg }

func TestStreamSamplesFilteredSkipsPayload(t *testing.T) {
	files := genFixtures(t, fixture.DefaultSpec())
	mf, err := parse.OpenMP4(files.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Close()
	ctx := context.Background()
	var withData, without int
	err = mf.StreamSamplesFiltered(ctx, func(s model.Sample) bool { return s.Seq%2 == 0 }, func(s model.Sample) error {
		if len(s.Data) == 0 {
			without++
		} else {
			withData++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if withData == 0 || without == 0 {
		t.Fatalf("过滤应同时产生有/无负载样本: %d/%d", withData, without)
	}
}

func TestContextCancelStopsStream(t *testing.T) {
	files := genFixtures(t, fixture.DefaultSpec())
	mf, err := parse.OpenMP4(files.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Close()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err = mf.StreamSamples(ctx, func(s model.Sample) error {
		calls++
		if calls == 5 {
			cancel()
		}
		return nil
	})
	if err == nil {
		t.Fatal("取消后应返回错误")
	}
	if calls > 10 {
		t.Fatalf("取消后仍继续读取了 %d 个样本", calls)
	}
}

func TestCorruptTruncationReportsOffset(t *testing.T) {
	files := genFixtures(t, fixture.DefaultSpec())
	_, err := parse.OpenMP4(files.Corrupt)
	if err == nil {
		t.Fatal("截断样例应解析失败")
	}
	fe, ok := fault.As(err)
	if !ok {
		t.Fatalf("错误应携带定位信息: %v", err)
	}
	if fe.Stage != fault.StageParse {
		t.Fatalf("阶段 %s", fe.Stage)
	}
	if fe.Offset <= 0 {
		t.Fatalf("应报告非零字节偏移，实际 %d", fe.Offset)
	}
}

func TestInvalidLengthBoxRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.mp4")
	// 一个 size=3 的非法箱（小于最小 8 字节头）。
	data := []byte{0, 0, 0, 3, 'f', 't', 'y', 'p', 0, 0, 0, 0}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := parse.OpenMP4(path)
	if err == nil {
		t.Fatal("非法箱长度应被拒绝")
	}
	if _, ok := fault.As(err); !ok {
		t.Fatalf("应返回 fault.Error: %v", err)
	}
}

func TestLoadTimelineRejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tl.json")
	if err := os.WriteFile(path, []byte(`{"version":"other/v9","tracks":[{"id":1,"timescale":90000}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := parse.LoadTimeline(path)
	if err == nil {
		t.Fatal("未知必需标记应被拒绝")
	}
}

func TestLoadTimelineRoundtrip(t *testing.T) {
	files := genFixtures(t, fixture.DefaultSpec())
	tl, err := parse.LoadTimeline(files.Sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Tracks) != 2 || len(tl.Clips) != 5 || len(tl.Transitions) != 1 {
		t.Fatalf("tracks=%d clips=%d transitions=%d", len(tl.Tracks), len(tl.Clips), len(tl.Transitions))
	}
	if tl.RTPTrackID != 1 {
		t.Fatalf("RTP 轨道 %d", tl.RTPTrackID)
	}
}
