package ebml

import (
	"errors"
	"io"
	"testing"

	"bcastqc/internal/media"
)

// ---- 测试用最小 EBML 编码器 ----

func encVInt(value uint64, width int) []byte {
	b := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		b[i] = byte(value & 0xFF)
		value >>= 8
	}
	b[0] |= 0x80 >> (width - 1)
	return b
}

func encID(id uint32) []byte {
	// 元素 ID 宽度由数值范围决定（标记位已包含在 id 常量中）。
	width := 4
	switch {
	case id >= 1<<24:
		width = 4
	case id >= 1<<16:
		width = 3
	case id >= 1<<8:
		width = 2
	default:
		width = 1
	}
	return encVInt(uint64(id), width)
}

func encSize(size int64) []byte {
	if size == UnknownSize {
		return []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	}
	switch {
	case size < 1<<7-1:
		return encVInt(uint64(size), 1)
	case size < 1<<14-1:
		return encVInt(uint64(size), 2)
	case size < 1<<21-1:
		return encVInt(uint64(size), 3)
	default:
		return encVInt(uint64(size), 4)
	}
}

func elem(id uint32, payload []byte) []byte {
	out := append(encID(id), encSize(int64(len(payload)))...)
	return append(out, payload...)
}

func elemUnknown(id uint32, payload []byte) []byte {
	out := append(encID(id), encSize(UnknownSize)...)
	return append(out, payload...)
}

func TestVIntWidths(t *testing.T) {
	cases := []struct {
		raw   []byte
		want  uint64
		width int
	}{
		{[]byte{0x82}, 2, 1},
		{[]byte{0x40, 0x02}, 2, 2},
		{[]byte{0x20, 0x00, 0x03}, 3, 3},
	}
	for _, c := range cases {
		r := NewBytesReader(c.raw)
		v, w, err := r.ReadVInt(false)
		if err != nil || v != c.want || w != c.width {
			t.Fatalf("got v=%d w=%d err=%v, want %d/%d", v, w, err, c.want, c.width)
		}
	}
}

func TestUnknownLengthParentBoundary(t *testing.T) {
	// 未知长度父元素包含两个已知长度子元素，EOF 自然收敛。
	child1 := elem(IDTrackNumber, []byte{1})
	child2 := elem(IDTrackType, []byte{TrackTypeAudio})
	doc := elemUnknown(IDTrackEntry, append(child1, child2...))
	r := NewBytesReader(doc)
	parent, err := r.Next()
	if err != nil {
		t.Fatalf("next parent: %v", err)
	}
	if parent.Size != UnknownSize {
		t.Fatalf("want unknown size, got %d", parent.Size)
	}
	sr := r.Child(parent)
	var ids []uint32
	for {
		el, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("child next: %v", err)
		}
		ids = append(ids, el.ID)
		if err := sr.Drain(el); err != nil {
			t.Fatalf("drain: %v", err)
		}
	}
	if len(ids) != 2 || ids[0] != IDTrackNumber || ids[1] != IDTrackType {
		t.Fatalf("unexpected children: %v", ids)
	}
}

func TestTruncationReportsFirstMissingOffset(t *testing.T) {
	good := elem(IDTrackNumber, []byte{7})
	// 声明 4 字节数据但只给 2 字节。
	trunc := append(encID(IDTrackType), encSize(4)...)
	trunc = append(trunc, 0x01, 0x02)
	doc := append(good, trunc...)
	r := NewBytesReader(doc)
	el, err := r.Next()
	if err != nil {
		t.Fatalf("first element: %v", err)
	}
	if err := r.Drain(el); err != nil {
		t.Fatalf("drain good: %v", err)
	}
	// 第二个元素头部可读，但其声明尺寸越过物理文件末尾。
	_, err = r.Next()
	var me *media.Error
	if !errors.As(err, &me) || me.Kind != media.KindTruncated {
		t.Fatalf("want truncated media.Error, got %v", err)
	}
	// 首个缺失字节即物理输入末尾。
	if me.Offset != int64(len(doc)) {
		t.Fatalf("offset=%d want %d", me.Offset, len(doc))
	}
}

func TestTruncatedHeaderReportsOffset(t *testing.T) {
	// 只有 1 个 ID 字节、尺寸 VINT 完全缺失。
	doc := []byte{0xD7}
	r := NewBytesReader(doc)
	_, err := r.Next()
	var me *media.Error
	if !errors.As(err, &me) || me.Kind != media.KindTruncated || me.Offset != 1 {
		t.Fatalf("want truncated at offset 1, got %v", err)
	}
}

func TestBoundaryOvershoot(t *testing.T) {
	// 父元素声明 2 字节，子元素却声明 10 字节数据。
	child := elem(IDTrackNumber, make([]byte, 10))
	parent := append(encID(IDTrackEntry), encSize(2)...)
	doc := append(parent, child...)
	r := NewBytesReader(doc)
	parentEl, err := r.Next()
	if err != nil {
		t.Fatalf("parent header: %v", err)
	}
	sr := r.Child(parentEl)
	_, err = sr.Next()
	if err == nil || !media.IsKind(err, media.KindBoundary) {
		t.Fatalf("want boundary error, got %v", err)
	}
}

func TestDecodeBlock(t *testing.T) {
	// 轨道 2（VINT 0x82），相对时间码 +5ms，关键帧标志，2 字节负载。
	data := []byte{0x82, 0x00, 0x05, 0x80, 0xAA, 0xBB}
	blk, err := decodeBlock(data, 100, media.MKTimecodeScale, true)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if blk.Track != 2 || !blk.Keyframe || blk.TimeNS != 105*media.MKTimecodeScale {
		t.Fatalf("unexpected block: %+v", blk)
	}
	if len(blk.Payload) != 2 || blk.Payload[0] != 0xAA {
		t.Fatalf("payload corrupted: %v", blk.Payload)
	}
}

func TestDecodeBlockNegativeRelativeTime(t *testing.T) {
	data := []byte{0x81, 0xFF, 0xFB, 0x00, 0x01} // 相对 -5
	blk, err := decodeBlock(data, 10, media.MKTimecodeScale, true)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if blk.TimeNS != 5*media.MKTimecodeScale {
		t.Fatalf("time=%d want 5ms", blk.TimeNS)
	}
}
