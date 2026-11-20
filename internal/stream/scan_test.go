package stream

import (
	"bytes"
	"testing"
)

// 合成夹具辅助：Annex-B 起始码。
var startCode4 = []byte{0, 0, 0, 1}

func h264NAL(nalType byte, forbidden bool, payload []byte) []byte {
	h := nalType & 0x1F
	if forbidden {
		h |= 0x80
	}
	out := append([]byte{}, startCode4...)
	out = append(out, h)
	return append(out, payload...)
}

func h265NAL(nalType byte, forbidden bool, payload []byte) []byte {
	h := (nalType & 0x3F) << 1
	if forbidden {
		h |= 0x80
	}
	out := append([]byte{}, startCode4...)
	out = append(out, h, 0) // 2 字节 NAL 头
	return append(out, payload...)
}

// av1OBU 构造低开销格式 OBU：header + leb128(size) + payload。
func av1OBU(obuType byte, keyframe bool, payload []byte, forbidden bool) []byte {
	h := (obuType & 0x0F) << 3
	h |= 0x02 // obu_has_size_field=1
	if forbidden {
		h |= 0x80
	}
	out := append([]byte{}, startCode4...)
	out = append(out, h, byte(len(payload)))
	p := append([]byte{}, payload...)
	if keyframe && len(p) > 0 {
		p[0] |= 0x80
	}
	return append(out, p...)
}

func TestScanH264AccessUnitsAndKeyframes(t *testing.T) {
	var stream []byte
	stream = append(stream, h264NAL(7, false, []byte("sps"))...) // SPS
	stream = append(stream, h264NAL(8, false, []byte("pps"))...) // PPS
	stream = append(stream, h264NAL(5, false, []byte("idr"))...) // IDR 关键帧
	stream = append(stream, h264NAL(1, false, []byte("p1"))...)
	stream = append(stream, h264NAL(1, false, []byte("p2"))...)

	res, err := NewScanner(H264, 0, 2).Scan(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AUs) != 3 {
		t.Fatalf("AU 数=%d, 期望 3 (SPS/PPS 附着到 IDR)", len(res.AUs))
	}
	if !res.AUs[0].Keyframe || res.AUs[1].Keyframe || res.AUs[2].Keyframe {
		t.Fatalf("关键帧标记错误: %+v", res.AUs)
	}
	// 首个 AU 必须从 SPS 起始码开始，覆盖到 IDR 结束。
	if res.AUs[0].Offset != 0 {
		t.Fatalf("首 AU 偏移=%d, 期望 0", res.AUs[0].Offset)
	}
	if len(res.Blocks) != 2 {
		t.Fatalf("块数=%d, 期望 2 (每块 2 帧)", len(res.Blocks))
	}
}

func TestScanH265KeyframeTypes(t *testing.T) {
	var stream []byte
	stream = append(stream, h265NAL(33, false, []byte("vps"))...)
	stream = append(stream, h265NAL(19, false, []byte("idr"))...) // IDR_W_RADL
	stream = append(stream, h265NAL(1, false, []byte("trail"))...)
	stream = append(stream, h265NAL(21, false, []byte("cra"))...) // CRA

	res, err := NewScanner(H265, 0, 10).Scan(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AUs) != 3 {
		t.Fatalf("AU 数=%d, 期望 3", len(res.AUs))
	}
	if !res.AUs[0].Keyframe || res.AUs[1].Keyframe || !res.AUs[2].Keyframe {
		t.Fatalf("H.265 关键帧识别错误: %+v", res.AUs)
	}
}

func TestScanAV1FrameOBUsAndKeyframes(t *testing.T) {
	var stream []byte
	stream = append(stream, av1OBU(obuSequenceHeader, false, []byte("seq"), false)...)
	stream = append(stream, av1OBU(obuFrame, true, []byte{0x00, 'k'}, false)...)
	stream = append(stream, av1OBU(obuFrame, false, []byte{0x00, 'p'}, false)...)
	stream = append(stream, av1OBU(obuMetadata, false, []byte("meta"), false)...)
	stream = append(stream, av1OBU(obuFrame, true, []byte{0x00, 'k'}, false)...)

	res, err := NewScanner(AV1, 0, 10).Scan(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AUs) != 3 {
		t.Fatalf("AV1 AU 数=%d, 期望 3", len(res.AUs))
	}
	if !res.AUs[0].Keyframe || res.AUs[1].Keyframe || !res.AUs[2].Keyframe {
		t.Fatalf("AV1 关键帧识别错误: %+v", res.AUs)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("正常流不应有错误, got %+v", res.Issues)
	}
}

func TestForbiddenNALBitLocatedExactly(t *testing.T) {
	var stream []byte
	stream = append(stream, h264NAL(5, false, []byte("idr"))...)
	corruptAt := int64(len(stream)) // 下一个 NAL 起始码之后的头字节
	stream = append(stream, h264NAL(1, true, []byte("bad"))...)

	res, err := NewScanner(H264, 0, 10).Scan(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	var found *Issue
	for i := range res.Issues {
		if res.Issues[i].Kind == IssueForbiddenNAL {
			found = &res.Issues[i]
		}
	}
	if found == nil {
		t.Fatalf("未检出 forbidden NAL 错误")
	}
	// 头字节 = 起始码(4) + 头(1)，问题偏移指向该 AU 起始码。
	if diff := found.Offset - corruptAt; diff < -4 || diff > 0 {
		t.Fatalf("定位偏差 %d 字节, 期望落在起始码到头部之间", diff)
	}
	if found.TrackID != 0 || found.Block < 0 {
		t.Fatalf("错误记录缺少轨道/块号: %+v", found)
	}
}

func TestIllegalAV1OBUForbiddenBit(t *testing.T) {
	var stream []byte
	stream = append(stream, av1OBU(obuFrame, true, []byte{0, 'k'}, false)...)
	stream = append(stream, av1OBU(obuFrame, false, []byte{0, 'p'}, true)...) // forbidden

	res, err := NewScanner(AV1, 0, 10).Scan(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 1 || res.Issues[0].Kind != IssueIllegalOBU {
		t.Fatalf("AV1 非法 OBU 检出错误: %+v", res.Issues)
	}
}

func TestTruncatedStreamReported(t *testing.T) {
	// 完整 NAL 之后追加半个起始码（两个零字节），模拟写入中断。
	stream := append(h264NAL(5, false, []byte("idr")), 0, 0)
	res, err := NewScanner(H264, 0, 10).Scan(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) == 0 || res.Issues[len(res.Issues)-1].Kind != IssueUnterminated {
		t.Fatalf("应报告流尾部不完整: %+v", res.Issues)
	}
}

func TestBlockChecksumsCoverFullStream(t *testing.T) {
	// 每帧净荷带序号，确保不同块内容与校验和不同。
	var stream []byte
	for i := 0; i < 20; i++ {
		stream = append(stream, h264NAL(1, false, []byte{byte(i) + 1, byte(i>>8) + 1})...)
	}
	res, err := NewScanner(H264, 1, 4).Scan(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Blocks) != 5 {
		t.Fatalf("块数=%d, 期望 5", len(res.Blocks))
	}
	var covered int64
	seen := map[string]bool{}
	for _, b := range res.Blocks {
		if b.Start != covered {
			t.Fatalf("块区间不连续: %+v", b)
		}
		covered = b.End
		if seen[b.SHA256] {
			t.Fatalf("块校验和重复，哈希实现异常")
		}
		seen[b.SHA256] = true
		if len(b.SHA256) != 64 {
			t.Fatalf("SHA-256 十六进制长度错误: %s", b.SHA256)
		}
	}
	if covered != res.Size {
		t.Fatalf("块覆盖=%d, 流长度=%d", covered, res.Size)
	}
}

func TestReadLEB128(t *testing.T) {
	if v, n := readLEB128([]byte{0x80, 0x01}); v != 128 || n != 2 {
		t.Fatalf("LEB128 多字节解析错误 v=%d n=%d", v, n)
	}
	if _, n := readLEB128([]byte{0x80}); n != 0 {
		t.Fatalf("截断 LEB128 应返回 n=0")
	}
}
