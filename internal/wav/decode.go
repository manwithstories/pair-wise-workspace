// Package wav parses PCM/WAV sample blocks into int32 interleaved PCM and
// rejects malformed containers with byte-accurate, classified errors.
package wav

import (
	"bytes"
	"encoding/binary"
	"io"

	"avcheck/internal/clock"
)

// PCM holds a decoded WAV payload as interleaved int32 samples.
type PCM struct {
	Channels     int
	BitsPerSamp  int
	SampleRate   uint32
	Samples      []int32 // interleaved, length = Frames*Channels
	DataOffset   int64   // byte offset where the data payload starts
}

// Frames returns the number of sample frames.
func (p *PCM) Frames() int {
	if p.Channels == 0 {
		return 0
	}
	return len(p.Samples) / p.Channels
}

type reader struct {
	buf []byte
	pos int64
}

func (r *reader) read(n int) ([]byte, error) {
	if r.pos+int64(n) > int64(len(r.buf)) {
		return nil, clock.NewErrorAt(clock.KindTruncatedWAV, r.pos,
			"need %d bytes, only %d remain", n, len(r.buf)-int(r.pos))
	}
	out := r.buf[r.pos : r.pos+int64(n)]
	r.pos += int64(n)
	return out, nil
}

func (r *reader) u16() (uint16, error) {
	b, err := r.read(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (r *reader) u32() (uint32, error) {
	b, err := r.read(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// Decode parses a complete RIFF/WAVE stream. Only PCM (format 1), 16/24 bit,
// 1-8 channels at clock.SampleRate are accepted.
func Decode(src io.Reader) (*PCM, error) {
	raw, err := io.ReadAll(src)
	if err != nil {
		return nil, clock.NewError(clock.KindTruncatedWAV, "read failed: %v", err)
	}
	r := &reader{buf: raw}

	if tag, e := r.read(4); e != nil {
		return nil, e
	} else if string(tag) != "RIFF" {
		return nil, clock.NewErrorAt(clock.KindTruncatedWAV, 0, "not a RIFF container")
	}
	riffSize, err := r.u32()
	if err != nil {
		return nil, err
	}
	if int64(riffSize)+8 > int64(len(raw)) {
		return nil, clock.NewErrorAt(clock.KindTruncatedWAV, 4,
			"RIFF size %d exceeds %d file bytes", riffSize, len(raw))
	}
	if tag, e := r.read(4); e != nil {
		return nil, e
	} else if string(tag) != "WAVE" {
		return nil, clock.NewErrorAt(clock.KindTruncatedWAV, 8, "not a WAVE stream")
	}

	p := &PCM{}
	var dataLen uint32
	var dataOff int64 = -1
	fmtSeen := false
	for r.pos < int64(len(raw)) {
		id, e := r.read(4)
		if e != nil {
			return nil, e
		}
		size, e := r.u32()
		if e != nil {
			return nil, e
		}
		bodyStart := r.pos
		if int64(size) > int64(len(raw))-r.pos {
			return nil, clock.NewErrorAt(clock.KindTruncatedWAV, bodyStart-4,
				"chunk %q size %d exceeds container", string(id), size)
		}
		switch string(id) {
		case "fmt ":
			fp, e := parseFmt(r, size)
			if e != nil {
				return nil, e
			}
			*p = *fp
			fmtSeen = true
		case "data":
			if !fmtSeen {
				return nil, clock.NewErrorAt(clock.KindTruncatedWAV, bodyStart-8,
					"data chunk before fmt chunk")
			}
			dataLen = size
			dataOff = r.pos
		}
		r.pos = bodyStart + int64(size) + int64(size&1) // pad to even
	}
	if !fmtSeen {
		return nil, clock.NewErrorAt(clock.KindTruncatedWAV, 12, "missing fmt chunk")
	}
	if dataOff < 0 {
		return nil, clock.NewErrorAt(clock.KindTruncatedWAV, int64(len(raw)), "missing data chunk")
	}
	return decodeData(r.buf, dataOff, dataLen, p)
}

func parseFmt(r *reader, size uint32) (*PCM, error) {
	if size < 16 {
		return nil, clock.NewErrorAt(clock.KindBadBlock, r.pos, "fmt chunk too short: %d", size)
	}
	format, err := r.u16()
	if err != nil {
		return nil, err
	}
	if format != 1 {
		return nil, clock.NewErrorAt(clock.KindBadBlock, r.pos-2, "unsupported format %d (PCM only)", format)
	}
	ch, err := r.u16()
	if err != nil {
		return nil, err
	}
	if ch < 1 || ch > 8 {
		return nil, clock.NewErrorAt(clock.KindBadBlock, r.pos-2, "channel count %d outside 1..8", ch)
	}
	rate, err := r.u32()
	if err != nil {
		return nil, err
	}
	if rate != clock.SampleRate {
		return nil, clock.NewErrorAt(clock.KindRateMismatch, r.pos-4,
			"sample rate %d != unified %d", rate, clock.SampleRate)
	}
	byteRate, err := r.u32()
	if err != nil {
		return nil, err
	}
	blockAlign, err := r.u16()
	if err != nil {
		return nil, err
	}
	bits, err := r.u16()
	if err != nil {
		return nil, err
	}
	if bits != 16 && bits != 24 {
		return nil, clock.NewErrorAt(clock.KindBadBlock, r.pos-2, "bit depth %d unsupported", bits)
	}
	if want := ch * bits / 8; blockAlign != want {
		return nil, clock.NewErrorAt(clock.KindBadBlock, r.pos-4,
			"block align %d != channels*bits/8 (%d)", blockAlign, want)
	}
	if byteRate != uint32(rate)*uint32(blockAlign) {
		return nil, clock.NewErrorAt(clock.KindBadBlock, r.pos-6,
			"byte rate %d inconsistent with rate/block align", byteRate)
	}
	return &PCM{Channels: int(ch), BitsPerSamp: int(bits), SampleRate: rate}, nil
}

func decodeData(buf []byte, off int64, length uint32, p *PCM) (*PCM, error) {
	align := p.Channels * p.BitsPerSamp / 8
	if length%uint32(align) != 0 {
		return nil, clock.NewErrorAt(clock.KindBadBlock, off,
			"data length %d not a multiple of block align %d", length, align)
	}
	if int64(off)+int64(length) > int64(len(buf)) {
		return nil, clock.NewErrorAt(clock.KindTruncatedWAV, off, "data payload truncated")
	}
	frameCount := int(length) / align
	p.Samples = make([]int32, frameCount*p.Channels)
	p.DataOffset = off
	src := buf[off : off+int64(length)]
	for i := 0; i < frameCount*p.Channels; i++ {
		switch p.BitsPerSamp {
		case 16:
			v := int16(binary.LittleEndian.Uint16(src[i*2:]))
			p.Samples[i] = int32(v)
		case 24:
			b0, b1, b2 := src[i*3], src[i*3+1], src[i*3+2]
			v := int32(b0) | int32(b1)<<8 | int32(b2)<<16
			p.Samples[i] = int32(int32(v) << 8 >> 8) // sign extend 24 bit
		}
	}
	return p, nil
}

// Encode serializes PCM as a canonical in-memory RIFF/WAVE stream. Used by
// locally generated fixtures only.
func Encode(p *PCM, bits int) []byte {
	align := p.Channels * bits / 8
	dataLen := len(p.Samples) * bits / 8
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(p.Channels))
	binary.Write(&b, binary.LittleEndian, uint32(clock.SampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(clock.SampleRate*uint32(align)))
	binary.Write(&b, binary.LittleEndian, uint16(align))
	binary.Write(&b, binary.LittleEndian, uint16(bits))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	for _, s := range p.Samples {
		switch bits {
		case 16:
			binary.Write(&b, binary.LittleEndian, int16(s))
		case 24:
			u := uint32(s) & 0xFFFFFF
			b.WriteByte(byte(u))
			b.WriteByte(byte(u >> 8))
			b.WriteByte(byte(u >> 16))
		}
	}
	return b.Bytes()
}
