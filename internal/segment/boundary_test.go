package segment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/abema/go-mp4"

	"archivecheck/internal/model"
)

func makePoints(n int, keyEvery int) []MediaPoint {
	pts := make([]MediaPoint, n)
	// 累计式纳秒：每 60 帧精确落在 2s 整数倍上，避免逐帧整除舍入累积。
	for i := range pts {
		pts[i] = MediaPoint{
			Seq:     uint32(i),
			IsKey:   i%keyEvery == 0,
			MediaNS: int64(i) * 2_000_000_000 / 60,
		}
	}
	return pts
}

func makeFrames(n, rep string, count int) []model.Frame {
	frames := make([]model.Frame, count)
	for i := range frames {
		pl := []byte(rep + ":" + string(rune(i)))
		sum := sha256.Sum256(pl)
		frames[i] = model.Frame{Seq: uint32(i), Payload: pl, PayloadHash: sum[:]}
	}
	return frames
}

func TestSelectBoundariesOnKeyframes(t *testing.T) {
	pts := makePoints(300, 30) // 30fps，每 30 帧一个关键帧
	cfg := BoundaryConfig{TargetNS: 2_000_000_000, ToleranceNS: 100_000_000, MinKeyGap: 1}
	bounds := SelectBoundaries(pts, cfg)
	if len(bounds) < 2 {
		t.Fatalf("应产生多个分片边界, got %d", len(bounds))
	}
	for _, b := range bounds {
		if !pts[b].IsKey {
			t.Fatalf("边界 %d 不在关键帧上", b)
		}
	}
	// 非末段的相邻边界间隔应接近 2s（60 帧）；末段可能是收尾残段。
	for i := 1; i < len(bounds)-1; i++ {
		gap := bounds[i] - bounds[i-1]
		if gap < 55 || gap > 65 {
			t.Fatalf("边界间隔偏离目标: %d", gap)
		}
	}
	// 收尾边界必须落在某个关键帧上且覆盖到输入尾部。
	if bounds[len(bounds)-1] <= bounds[len(bounds)-2] {
		t.Fatal("收尾边界必须严格递增")
	}
}

func TestBuildTailIncomplete(t *testing.T) {
	pts := makePoints(100, 30)
	frames := makeFrames("r", "r", 100)
	cfg := BoundaryConfig{TargetNS: 2_000_000_000, ToleranceNS: 100_000_000, MinKeyGap: 1}
	bounds := SelectBoundaries(pts, cfg)
	m := Build("r", frames, pts, bounds, cfg)
	if len(m.Segments) != len(bounds) {
		t.Fatalf("分片数错误: %d/%d", len(m.Segments), len(bounds))
	}
	last := m.Segments[len(m.Segments)-1]
	if !last.TailIncomplete {
		t.Fatal("末尾短分片必须标记 TailIncomplete")
	}
	// 非尾分片不得带尾标记。
	for _, s := range m.Segments[:len(m.Segments)-1] {
		if s.TailIncomplete {
			t.Fatalf("分片 %d 不应标记尾不完整", s.Index)
		}
		if len(s.PayloadSHA256) != sha256.Size {
			t.Fatal("分片摘要长度错误")
		}
	}
}

func TestBuildFullDurationTailNotFlagged(t *testing.T) {
	// 单片覆盖时长达到/超过目标（62 帧 ≈ 2.067s）时不得误标尾不完整。
	pts := makePoints(62, 30)
	frames := makeFrames("r", "r", 62)
	cfg := BoundaryConfig{TargetNS: 2_000_000_000, ToleranceNS: 100_000_000, MinKeyGap: 1}
	m := Build("r", frames, pts, SelectBoundaries(pts, cfg), cfg)
	if m.Segments[0].TailIncomplete {
		t.Fatal("达到目标时长的尾分片不应标记不完整")
	}
}

func TestManifestHashDeterministic(t *testing.T) {
	pts := makePoints(120, 30)
	frames := makeFrames("r", "r", 120)
	cfg := BoundaryConfig{TargetNS: 2_000_000_000, ToleranceNS: 100_000_000, MinKeyGap: 1}
	m1 := Build("r", frames, pts, SelectBoundaries(pts, cfg), cfg)
	m2 := Build("r", frames, pts, SelectBoundaries(pts, cfg), cfg)
	h1, h2 := Hash(m1), Hash(m2)
	if h1 != h2 {
		t.Fatal("相同输入的清单哈希必须一致")
	}
	if len(hex.EncodeToString(h1[:])) != 64 {
		t.Fatal("清单摘要应为 32 字节")
	}
}

func TestWriteMP4AssociationRoundTrip(t *testing.T) {
	pts := makePoints(60, 30)
	frames := makeFrames("r", "r", 60)
	cfg := BoundaryConfig{TargetNS: 2_000_000_000, ToleranceNS: 100_000_000, MinKeyGap: 1}
	m := Build("r", frames, pts, SelectBoundaries(pts, cfg), cfg)
	digest := Hash(m)

	var mem memFile
	if err := WriteMP4Association(&mem, m); err != nil {
		t.Fatal(err)
	}
	raw := mem.buf
	if !bytes.Contains(raw, []byte("ftyp")) || !bytes.Contains(raw, []byte("moov")) {
		t.Fatal("MP4 容器缺少 ftyp/moov box")
	}
	if !bytes.Contains(raw, digest[:]) {
		t.Fatal("容器内未嵌入清单摘要")
	}
	// 用 go-mp4 解析器验证结构可被标准读取；未知摘要 box 不展开，其余递归。
	boxCount := 0
	if _, err := mp4.ReadBoxStructure(bytes.NewReader(raw), func(h *mp4.ReadHandle) (interface{}, error) {
		boxCount++
		if !h.BoxInfo.IsSupportedType() {
			return nil, nil
		}
		return h.Expand()
	}); err != nil {
		t.Fatalf("MP4 结构解析失败: %v", err)
	}
	if boxCount < 4 { // ftyp / moov / udta / mhsd
		t.Fatalf("解析出的 box 数量异常: %d", boxCount)
	}
}

// memFile 是支持任意偏移 Seek/Write 的内存文件，供 go-mp4 关 box 时回填 size。
type memFile struct {
	buf []byte
	pos int64
}

func (m *memFile) Write(p []byte) (int, error) {
	if end := m.pos + int64(len(p)); end > int64(len(m.buf)) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:], p)
	m.pos += int64(len(p))
	return len(p), nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		m.pos = offset
	case 1:
		m.pos += offset
	case 2:
		m.pos = int64(len(m.buf)) + offset
	}
	return m.pos, nil
}
