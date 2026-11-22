package wav

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

// buildWAV 构造一个最小合法 WAV：PCM 或 float，可注入畸形选项。
type wavOpt struct {
	formatTag  uint16
	extensible bool
	channels   uint16
	bits       uint16
	rate       uint32
	data       []byte
	truncateAt int    // >0 时在该字节处截断整个流
	badRIFF    bool   // 替换 RIFF 魔数
	badFmtSize uint32 // >0 时覆盖 fmt 块长度
	badDataSize uint32 // >0 时覆盖 data 块声明长度
	oversizeChunk bool // 声明 >1MiB 的块
	dangleTail  bool  // 追加一个无法成帧的字节
}

func buildWAV(o wavOpt) []byte {
	tag := o.formatTag
	if tag == 0 {
		tag = FormatPCM
	}
	ch := o.channels
	if ch == 0 {
		ch = 2
	}
	bits := o.bits
	if bits == 0 {
		bits = 16
	}
	rate := o.rate
	if rate == 0 {
		rate = 48000
	}
	frame := uint64(ch) * uint64(bits) / 8
	var fmtBody []byte
	if o.extensible {
		fmtBody = make([]byte, 16+2+2+4+16)
		binary.LittleEndian.PutUint16(fmtBody[0:2], FormatExt)
		cb := uint16(2 + 2 + 4 + 16)
		binary.LittleEndian.PutUint16(fmtBody[16:18], cb)
		binary.LittleEndian.PutUint16(fmtBody[16+2+2+4:], tag)
	} else {
		fmtBody = make([]byte, 16)
		binary.LittleEndian.PutUint16(fmtBody[0:2], tag)
	}
	binary.LittleEndian.PutUint16(fmtBody[2:4], ch)
	binary.LittleEndian.PutUint32(fmtBody[4:8], rate)
	binary.LittleEndian.PutUint32(fmtBody[8:12], uint32(frame)*rate)
	binary.LittleEndian.PutUint16(fmtBody[12:14], uint16(frame))
	binary.LittleEndian.PutUint16(fmtBody[14:16], bits)

	var b bytes.Buffer
	writeChunk := func(id string, body []byte, declared uint32) {
		b.WriteString(id)
		binary.Write(&b, binary.LittleEndian, declared)
		b.Write(body)
		if len(body)%2 == 1 {
			b.WriteByte(0)
		}
	}
	fmtDecl := uint32(len(fmtBody))
	if o.badFmtSize > 0 {
		fmtDecl = o.badFmtSize
	}
	writeChunk("fmt ", fmtBody, fmtDecl)

	if o.oversizeChunk {
		b.WriteString("junk")
		binary.Write(&b, binary.LittleEndian, uint32(MaxChunkBytes+1))
	}

	dataDecl := uint32(len(o.data))
	if o.badDataSize > 0 {
		dataDecl = o.badDataSize
	}
	writeChunk("data", o.data, dataDecl)
	if o.dangleTail {
		b.WriteByte(0xAB)
	}

	out := b.Bytes()
	riff := []byte("RIFF")
	if o.badRIFF {
		riff = []byte("RIFX")
	}
	head := make([]byte, 12)
	copy(head[0:4], riff)
	binary.LittleEndian.PutUint32(head[4:8], uint32(len(out))+4)
	copy(head[8:12], "WAVE")
	full := append(head, out...)
	if o.truncateAt > 0 && o.truncateAt < len(full) {
		full = full[:o.truncateAt]
	}
	return full
}

func pcmData(frames int) []byte {
	d := make([]byte, frames*4) // 2ch * 16bit
	for i := range d {
		d[i] = byte(i)
	}
	return d
}

func TestParseHappyPathPCM(t *testing.T) {
	raw := buildWAV(wavOpt{data: pcmData(8)})
	p := NewParser(bytes.NewReader(raw))
	if err := p.ParseHeader(); err != nil {
		t.Fatalf("header: %v", err)
	}
	id, err := p.NextChunk()
	if err != nil || id != "fmt " {
		t.Fatalf("next chunk id=%q err=%v", id, err)
	}
	f, err := p.Format()
	if err != nil || !f.IsPCM() || f.Channels != 2 {
		t.Fatalf("format: %+v err=%v", f, err)
	}
	if id, err = p.NextChunk(); err != nil || id != "data" {
		t.Fatalf("data chunk id=%q err=%v", id, err)
	}
	var total uint64
	for {
		blk, err := p.ReadData(16) // 每次 4 帧
		total += blk.FrameCount
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read data: %v", err)
		}
		if blk.FrameCount != 4 {
			t.Fatalf("block frames=%d", blk.FrameCount)
		}
	}
	if total != 8 || p.FramesDelivered() != 8 {
		t.Fatalf("frames total=%d delivered=%d", total, p.FramesDelivered())
	}
}

func TestParseFloatAndExtensible(t *testing.T) {
	raw := buildWAV(wavOpt{formatTag: FormatFloat, extensible: true, bits: 32, data: make([]byte, 4*2*3)})
	p := NewParser(bytes.NewReader(raw))
	if err := p.ParseHeader(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NextChunk(); err != nil {
		t.Fatal(err)
	}
	f, _ := p.Format()
	if !f.IsFloat() || f.FormatTag != FormatExt || f.ExtFormatTag != FormatFloat {
		t.Fatalf("extensible float not recognized: %+v", f)
	}
}

func TestRejectBadRIFF(t *testing.T) {
	raw := buildWAV(wavOpt{badRIFF: true, data: pcmData(2)})
	if err := NewParser(bytes.NewReader(raw)).ParseHeader(); err == nil {
		t.Fatal("bad RIFF magic must fail")
	}
}

func TestRejectTruncatedHeader(t *testing.T) {
	raw := buildWAV(wavOpt{truncateAt: 7, data: pcmData(2)})
	if err := NewParser(bytes.NewReader(raw)).ParseHeader(); err == nil {
		t.Fatal("truncated header must fail")
	}
}

func TestRejectTruncatedData(t *testing.T) {
	raw := buildWAV(wavOpt{data: pcmData(8), badDataSize: 64}) // 声明 64 实际 32
	p := NewParser(bytes.NewReader(raw))
	_ = p.ParseHeader()
	if _, err := p.NextChunk(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NextChunk(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadData(64); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("expected truncation error, got %v", err)
	}
}

func TestRejectIncompleteFrame(t *testing.T) {
	raw := buildWAV(wavOpt{data: pcmData(2), dangleTail: true})
	p := NewParser(bytes.NewReader(raw))
	_ = p.ParseHeader()
	_, _ = p.NextChunk()
	_, _ = p.NextChunk()
	if _, err := p.ReadData(64); err == nil || !strings.Contains(err.Error(), "dangling") {
		t.Fatalf("expected dangling frame error, got %v", err)
	}
}

func TestRejectIllegalChannels(t *testing.T) {
	raw := buildWAV(wavOpt{channels: 0, data: pcmData(2)})
	// buildWAV 会把 0 修正为 2，这里手工构造非法声道。
	raw = buildWAV(wavOpt{data: pcmData(2)})
	raw = withChannels(raw, 0)
	p := NewParser(bytes.NewReader(raw))
	_ = p.ParseHeader()
	if _, err := p.NextChunk(); err == nil {
		t.Fatal("zero channels must be rejected")
	}
}

func TestRejectOversizeChunk(t *testing.T) {
	raw := buildWAV(wavOpt{oversizeChunk: true, data: pcmData(2)})
	p := NewParser(bytes.NewReader(raw))
	_ = p.ParseHeader()
	_, _ = p.NextChunk()
	if _, err := p.NextChunk(); err == nil || !strings.Contains(err.Error(), "1MiB") {
		t.Fatalf("expected oversize chunk error, got %v", err)
	}
}

func TestRejectBadBlockAlign(t *testing.T) {
	raw := buildWAV(wavOpt{data: pcmData(2)})
	// block align 位于 RIFF 头之后 fmt 负载偏移 12。
	raw[12+8+12] = 0xFF
	p := NewParser(bytes.NewReader(raw))
	_ = p.ParseHeader()
	if _, err := p.NextChunk(); err == nil {
		t.Fatal("inconsistent block align must be rejected")
	}
}

// withChannels 覆盖 fmt 负载中的声道数字段。
func withChannels(raw []byte, ch uint16) []byte {
	out := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint16(out[12+8+2:], ch)
	return out
}
