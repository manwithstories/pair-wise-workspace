package fixture

import (
	"bytes"
	"testing"

	"media-audit/internal/clock"
	"media-audit/internal/sidecar"
	"media-audit/internal/stream"
)

func specFor(codec stream.Codec) Spec {
	return Spec{
		Codec:         codec,
		TrackID:       0,
		Frames:        600,
		Timescale:     90000,
		NominalStep:   3000,
		KeyframeEvery: 150,
		Seed:          42,
	}
}

func TestBuildStreamScannableForAllCodecs(t *testing.T) {
	for _, codec := range []stream.Codec{stream.H264, stream.H265, stream.AV1} {
		spec := specFor(codec)
		data, frames := BuildStream(spec)
		if len(frames) != 600 {
			t.Fatalf("%s 帧数=%d", codec, len(frames))
		}
		res, err := stream.NewScanner(codec, 0, 100).Scan(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s 扫描失败: %v", codec, err)
		}
		if len(res.AUs) != 600 {
			t.Fatalf("%s AU 数=%d, 期望 600", codec, len(res.AUs))
		}
		if len(res.Issues) != 0 {
			t.Fatalf("%s 干净流不应有错误: %+v", codec, res.Issues)
		}
		// 每 150 帧一个关键帧 => 4 个。
		kf := 0
		for _, au := range res.AUs {
			if au.Keyframe {
				kf++
			}
		}
		if kf != 4 {
			t.Fatalf("%s 关键帧数=%d, 期望 4", codec, kf)
		}
	}
}

func TestCorruptionLocatedExactly(t *testing.T) {
	for _, codec := range []stream.Codec{stream.H264, stream.H265, stream.AV1} {
		spec := specFor(codec)
		data, _ := BuildStream(spec)
		c := InjectCorruptionAtFrame(codec, data, 300)

		res, err := stream.NewScanner(codec, 0, 100).Scan(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		var found *stream.Issue
		for i := range res.Issues {
			if res.Issues[i].Kind == c.Kind {
				found = &res.Issues[i]
			}
		}
		if found == nil {
			t.Fatalf("%s 未检出注入损坏 %s", codec, c.Kind)
		}
		// 损坏字节落在问题单元起始码到 AU 结束区间内，起始码误差 <=4 字节。
		if c.Offset < found.Offset || c.Offset > found.End {
			t.Fatalf("%s 损坏点 %d 不在问题区间 [%d,%d]", codec, c.Offset, found.Offset, found.End)
		}
		if c.Offset-found.Offset > 4 {
			t.Fatalf("%s 定位误差 %d 字节, 期望 <=4", codec, c.Offset-found.Offset)
		}
		// 帧 300、每块 100 帧：H.264/H.265 落在块 3；AV1 因首 AU 前有
		// 序列头，块号允许为 2 或 3，但必须确定且非负。
		if found.Block < 2 || found.Block > 3 {
			t.Fatalf("%s 损坏块号=%d 超出预期范围", codec, found.Block)
		}
	}
}

func TestBuildMetaAndSidecarRoundTrip(t *testing.T) {
	spec := specFor(stream.AV1)
	data, frames := BuildStream(spec)
	meta := BuildMeta(spec, data, frames)
	if meta.StreamSize != int64(len(data)) || len(meta.StreamSHA256) != 64 {
		t.Fatalf("元数据摘要错误: %+v", meta)
	}
	raw, err := sidecar.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sidecar.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Frames) != 600 || got.StreamSHA256 != meta.StreamSHA256 {
		t.Fatalf("sidecar 往返不一致")
	}
}

func TestDeterministicGeneration(t *testing.T) {
	a, fa := BuildStream(specFor(stream.H264))
	b, fb := BuildStream(specFor(stream.H264))
	if !bytes.Equal(a, b) {
		t.Fatalf("相同规格两次生成的码流不一致")
	}
	if fa[10].RawTS != fb[10].RawTS {
		t.Fatalf("时间戳不确定")
	}
}

func TestTruncateStreamDetected(t *testing.T) {
	spec := specFor(stream.H264)
	data, _ := BuildStream(spec)
	// 在第 200 帧中间截断并追加未写完的起始码。
	cut := len(data) / 2
	trunc := TruncateStream(data, cut)
	if len(trunc) != cut+2 {
		t.Fatalf("截断长度=%d, 期望 %d", len(trunc), cut+2)
	}
	res, err := stream.NewScanner(stream.H264, 0, 100).Scan(bytes.NewReader(trunc))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range res.Issues {
		if is.Kind == stream.IssueUnterminated {
			found = true
		}
	}
	if !found {
		t.Fatalf("截断流必须报告 unterminated 诊断")
	}
}

func TestWraparoundAndVFRPatterns(t *testing.T) {
	ts := make([]FrameTS, 10)
	WrapTimestamps(ts, 0xFFFFFFF0, 3000, 5)
	// 裸 32 位值在回绕点变小，但用回绕展开后必须恢复单调。
	var prev64 uint64
	var sawWrap bool
	first := true
	for i, f := range ts {
		if first {
			prev64 = uint64(f.Raw)
			first = false
			continue
		}
		out, wrapped, backward := clock.Unwrap32(prev64, f.Raw)
		if backward {
			t.Fatalf("第 %d 帧被误判为反向跳变", i)
		}
		if wrapped {
			sawWrap = true
		}
		if out <= prev64 {
			t.Fatalf("展开后时间轴非单调: %d -> %d", prev64, out)
		}
		prev64 = out
	}
	if !sawWrap {
		t.Fatalf("未观察到 32 位回绕事件")
	}
	pat := VFRPattern(3000, 2.0)
	if len(pat) != 4 || pat[3] != 6000 {
		t.Fatalf("VFR 模式错误: %v", pat)
	}
	drift := DriftPattern(3000, 1000, 4)
	if drift[0] != 3003 {
		t.Fatalf("漂移步长=%d, 期望 3003", drift[0])
	}
}
