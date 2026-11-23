package matroska

import (
	"bytes"
	"errors"
	"testing"

	"mediakernel/internal/clock"
	"mediakernel/internal/ebml"
)

// 测试用最小 EBML 写入器。
func wID(buf *bytes.Buffer, id uint32) {
	switch {
	case id > 0xFFFFFF:
		buf.WriteByte(byte(id >> 24))
		fallthrough
	case id > 0xFFFF:
		buf.WriteByte(byte(id >> 16))
		fallthrough
	case id > 0xFF:
		buf.WriteByte(byte(id >> 8))
		fallthrough
	default:
		buf.WriteByte(byte(id))
	}
}

func wSize(buf *bytes.Buffer, size int) {
	switch {
	case size < 0x7F:
		buf.WriteByte(byte(0x80 | size))
	case size < 0x3FFF:
		buf.WriteByte(byte(0x40 | size>>8))
		buf.WriteByte(byte(size))
	default:
		buf.WriteByte(byte(0x20 | size>>16))
		buf.WriteByte(byte(size >> 8))
		buf.WriteByte(byte(size))
	}
}

func wElem(buf *bytes.Buffer, id uint32, payload []byte) {
	wID(buf, id)
	wSize(buf, len(payload))
	buf.Write(payload)
}

func wUint(buf *bytes.Buffer, id uint32, v uint64) {
	var p []byte
	for x := v; ; x >>= 8 {
		p = append([]byte{byte(x)}, p...)
		if x < 256 {
			break
		}
	}
	wElem(buf, id, p)
}

func wBlock(buf *bytes.Buffer, track byte, rel int16, key bool, payload []byte) {
	var p []byte
	p = append(p, 0x80|track, byte(uint16(rel)>>8), byte(uint16(rel)))
	if key {
		p = append(p, 0x80)
	} else {
		p = append(p, 0x00)
	}
	p = append(p, payload...)
	wElem(buf, ebml.IDSimpleBlock, p)
}

// 构造未知长度 Segment/Cluster、双轨交错块，
// 含一个负时间戳块与一个非单调块。
func buildWebM() []byte {
	var b bytes.Buffer
	wElem(&b, ebml.IDEBML, []byte{0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'})
	// Segment 未知长度
	wID(&b, ebml.IDSegment)
	b.WriteByte(0xFF)
	// Info: TimecodeScale = 1ms
	var info bytes.Buffer
	wUint(&info, ebml.IDTimecodeScale, 1_000_000)
	wElem(&b, ebml.IDInfo, info.Bytes())
	// Tracks: 1=video, 2=audio
	var tracks bytes.Buffer
	for _, tr := range []struct{ num, typ uint64; codec string }{
		{1, TrackVideo, "V_VP8"}, {2, TrackAudio, "A_OPUS"},
	} {
		var e bytes.Buffer
		wUint(&e, ebml.IDTrackNumber, tr.num)
		wUint(&e, ebml.IDTrackType, tr.typ)
		wElem(&e, ebml.IDCodecID, []byte(tr.codec))
		wElem(&tracks, ebml.IDTrackEntry, e.Bytes())
	}
	wElem(&b, ebml.IDTracks, tracks.Bytes())
	// Cluster 1 (timecode 0)，未知长度
	wID(&b, ebml.IDCluster)
	b.WriteByte(0xFF)
	wUint(&b, ebml.IDTimecode, 0)
	wBlock(&b, 1, 0, true, []byte("v0"))
	wBlock(&b, 2, 0, false, []byte("a0"))
	wBlock(&b, 1, 33, false, []byte("v1"))
	wBlock(&b, 2, 20, false, []byte("a1"))
	wBlock(&b, 1, -5, false, []byte("neg"))  // 负绝对时间戳
	wBlock(&b, 1, 10, false, []byte("mono")) // 非单调（上一个有效值 33ms）
	// Cluster 2 (timecode 500)
	wID(&b, ebml.IDCluster)
	b.WriteByte(0xFF)
	wUint(&b, ebml.IDTimecode, 500)
	wBlock(&b, 1, 0, true, []byte("v2"))
	wBlock(&b, 2, 0, false, []byte("a2"))
	return b.Bytes()
}

type collector struct {
	tracks   []Track
	blocks   []Block
	problems []error
}

func (c *collector) Tracks(t []Track) { c.tracks = t }
func (c *collector) Block(b Block)    { c.blocks = append(c.blocks, b) }
func (c *collector) Problem(err error) {
	c.problems = append(c.problems, err)
}

func TestParseInterleaved(t *testing.T) {
	var c collector
	err := Parse(ebml.NewReader(bytes.NewReader(buildWebM())), &c)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.tracks) != 2 || c.tracks[0].Codec != "V_VP8" || c.tracks[1].Type != TrackAudio {
		t.Fatalf("tracks: %+v", c.tracks)
	}
	if len(c.blocks) != 8 {
		t.Fatalf("blocks=%d", len(c.blocks))
	}
	// 交错顺序保持
	want := []struct {
		track uint64
		ms    int64
		data  string
	}{
		{1, 0, "v0"}, {2, 0, "a0"}, {1, 33, "v1"}, {2, 20, "a1"},
		{1, -5, "neg"}, {1, 10, "mono"}, {1, 500, "v2"}, {2, 500, "a2"},
	}
	for i, w := range want {
		b := c.blocks[i]
		if b.Track != w.track || int64(b.Abs)/1_000_000 != w.ms || string(b.Data) != w.data {
			t.Fatalf("block %d: track=%d abs=%d data=%q", i, b.Track, b.Abs, b.Data)
		}
	}
	if !c.blocks[0].Keyframe || c.blocks[1].Keyframe {
		t.Fatal("keyframe flags wrong")
	}
	// 统一错误模型：负时间戳 + 非单调
	var kinds []clock.ErrKind
	for _, p := range c.problems {
		var se *clock.StampError
		if errors.As(p, &se) {
			kinds = append(kinds, se.Kind)
		}
	}
	if len(kinds) != 2 || kinds[0] != clock.ErrNegativeTimestamp || kinds[1] != clock.ErrNonMonotonic {
		t.Fatalf("problems: %v", c.problems)
	}
	// 负时间戳块未被改写
	if c.blocks[4].Abs != -5*clock.Millisecond {
		t.Fatalf("negative block rewritten: %d", c.blocks[4].Abs)
	}
}

func TestParseTruncatedBlock(t *testing.T) {
	full := buildWebM()
	cut := full[:len(full)-3] // 截断最后一个 SimpleBlock
	var c collector
	err := Parse(ebml.NewReader(bytes.NewReader(cut)), &c)
	var be *BlockError
	if !errors.As(err, &be) {
		t.Fatalf("want BlockError, got %v", err)
	}
	var te *ebml.TruncatedError
	if !errors.As(err, &te) {
		t.Fatalf("want TruncatedError inside, got %v", err)
	}
	if be.RecoverAt <= 0 || te.Offset <= be.RecoverAt {
		t.Fatalf("block error: %+v", be)
	}
}

func TestLacingRejected(t *testing.T) {
	var b bytes.Buffer
	wID(&b, ebml.IDSegment)
	b.WriteByte(0xFF)
	wID(&b, ebml.IDCluster)
	b.WriteByte(0xFF)
	wUint(&b, ebml.IDTimecode, 0)
	// 带 Xiph 花边标志的块
	wElem(&b, ebml.IDSimpleBlock, []byte{0x81, 0x00, 0x00, 0x02, 0x01, 0xAA})
	var c collector
	if err := Parse(ebml.NewReader(bytes.NewReader(b.Bytes())), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.blocks) != 0 || len(c.problems) != 1 {
		t.Fatalf("blocks=%d problems=%d", len(c.blocks), len(c.problems))
	}
	var be *BlockError
	if !errors.As(c.problems[0], &be) || !errors.Is(be.Err, ErrUnsupportedLacing) {
		t.Fatalf("problem: %v", c.problems[0])
	}
}
