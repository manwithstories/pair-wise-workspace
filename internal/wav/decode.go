package wav

import (
	"crypto/sha256"
	"encoding/binary"

	"avcheck/internal/fault"
)

// PCM is decoded interleaved signed-16 audio.
type PCM struct {
	Rate    int
	Layout  Layout
	Samples []int16 // interleaved
}

// Frames returns the number of sample frames.
func (p *PCM) Frames() int {
	if p.Layout.Channels() == 0 {
		return 0
	}
	return len(p.Samples) / p.Layout.Channels()
}

// DecodeWAV parses a RIFF/WAVE 16-bit PCM file. Truncated headers and
// non-integral sample frames are rejected as unsupported input.
func DecodeWAV(data []byte) (*PCM, error) {
	const op = "wav.decode"
	if len(data) < 44 {
		return nil, fault.New(fault.Unsupported, op, int64(len(data)), "truncated WAV header")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fault.New(fault.Unsupported, op, 0, "bad RIFF/WAVE magic")
	}
	pos := 12
	var rate, channels, bits int
	var payload []byte
	for pos+8 <= len(data) {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		body := pos + 8
		if body+size > len(data) {
			return nil, fault.New(fault.Unsupported, op, int64(pos), "truncated chunk "+id)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, fault.New(fault.Unsupported, op, int64(pos), "short fmt chunk")
			}
			if binary.LittleEndian.Uint16(data[body:]) != 1 {
				return nil, fault.New(fault.Unsupported, op, int64(pos), "not PCM format")
			}
			channels = int(binary.LittleEndian.Uint16(data[body+2:]))
			rate = int(binary.LittleEndian.Uint32(data[body+4:]))
			bits = int(binary.LittleEndian.Uint16(data[body+14:]))
		case "data":
			payload = data[body : body+size]
		}
		pos = body + size + (size & 1)
	}
	if payload == nil || channels == 0 {
		return nil, fault.New(fault.Unsupported, op, int64(pos), "missing fmt or data chunk")
	}
	if bits != 16 {
		return nil, fault.New(fault.Unsupported, op, 0, "only 16-bit samples supported")
	}
	frame := channels * 2
	if len(payload)%frame != 0 {
		return nil, fault.New(fault.Unsupported, op, int64(len(payload)),
			"data size not a whole number of frames")
	}
	s := make([]int16, len(payload)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(payload[i*2:]))
	}
	return &PCM{Rate: rate, Layout: LayoutFromChannels(channels), Samples: s}, nil
}

// LayoutFromChannels picks the layout matching a channel count.
func LayoutFromChannels(n int) Layout {
	for _, l := range []Layout{Mono, Stereo, Quad, Surround51} {
		if l.Channels() == n {
			return l
		}
	}
	return LayoutUnknown
}

// EncodeWAV serializes PCM as a 16-bit RIFF/WAVE file.
func EncodeWAV(p *PCM) []byte {
	ch := p.Layout.Channels()
	data := make([]byte, len(p.Samples)*2)
	for i, s := range p.Samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}
	out := make([]byte, 44)
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(data)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], uint16(ch))
	binary.LittleEndian.PutUint32(out[24:], uint32(p.Rate))
	binary.LittleEndian.PutUint32(out[28:], uint32(p.Rate*ch*2))
	binary.LittleEndian.PutUint16(out[32:], uint16(ch*2))
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(data)))
	return append(out, data...)
}

// Redundancy carries a copy of an earlier block's samples.
type Redundancy struct {
	Seq     uint64
	Samples []int16
}

// Block is one network sample block with two levels of redundancy.
type Block struct {
	Seq     uint64
	Track   int
	Rate    int
	Layout  Layout
	Samples []int16
	Red     [2]Redundancy // Red[0]=previous block, Red[1]=the one before
}

const noneSeq = ^uint64(0)

// EncodeBlock serializes a block; the trailing SHA-256 covers all prior bytes.
func EncodeBlock(b *Block) []byte {
	buf := make([]byte, 0, 64+len(b.Samples)*2)
	buf = append(buf, "AVBK"...)
	buf = binary.LittleEndian.AppendUint64(buf, b.Seq)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(b.Track))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(b.Rate))
	buf = append(buf, b.Layout.Tag())
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(b.Samples)))
	for _, s := range b.Samples {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(s))
	}
	for _, r := range b.Red {
		if r.Samples == nil {
			buf = binary.LittleEndian.AppendUint64(buf, noneSeq)
			continue
		}
		buf = binary.LittleEndian.AppendUint64(buf, r.Seq)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(r.Samples)))
		for _, s := range r.Samples {
			buf = binary.LittleEndian.AppendUint16(buf, uint16(s))
		}
	}
	sum := sha256.Sum256(buf)
	return append(buf, sum[:]...)
}

// DecodeBlock parses and verifies a wire block. Digest mismatches and
// truncation are recoverable corruption; unknown layouts are unsupported.
func DecodeBlock(data []byte) (*Block, error) {
	const op = "wav.block"
	if len(data) < 4+8+2+4+1+4+32 {
		return nil, fault.New(fault.Corrupt, op, int64(len(data)), "truncated block")
	}
	if string(data[:4]) != "AVBK" {
		return nil, fault.New(fault.Corrupt, op, 0, "bad block magic")
	}
	body, digest := data[:len(data)-32], data[len(data)-32:]
	if sha256.Sum256(body) != [32]byte(digest) {
		return nil, fault.New(fault.Corrupt, op, 0, "block digest mismatch")
	}
	b := &Block{}
	b.Seq = binary.LittleEndian.Uint64(body[4:])
	b.Track = int(binary.LittleEndian.Uint16(body[12:]))
	b.Rate = int(binary.LittleEndian.Uint32(body[14:]))
	lay, err := LayoutFromTag(body[18])
	if err != nil {
		return nil, err
	}
	b.Layout = lay
	off := 19
	n := int(binary.LittleEndian.Uint32(body[off:]))
	off += 4
	ch := lay.Channels()
	if ch == 0 || n%(ch) != 0 {
		return nil, fault.New(fault.Unsupported, op, int64(b.Seq), "non-integral frame count")
	}
	read := func(dst *[]int16, count int) error {
		if off+count*2 > len(body) {
			return fault.New(fault.Corrupt, op, int64(off), "truncated block payload")
		}
		s := make([]int16, count)
		for i := range s {
			s[i] = int16(binary.LittleEndian.Uint16(body[off+i*2:]))
		}
		off += count * 2
		*dst = s
		return nil
	}
	if err := read(&b.Samples, n); err != nil {
		return nil, err
	}
	for i := range b.Red {
		if off+8 > len(body) {
			return nil, fault.New(fault.Corrupt, op, int64(off), "truncated redundancy")
		}
		seq := binary.LittleEndian.Uint64(body[off:])
		off += 8
		if seq == noneSeq {
			continue
		}
		if off+4 > len(body) {
			return nil, fault.New(fault.Corrupt, op, int64(off), "truncated redundancy")
		}
		rn := int(binary.LittleEndian.Uint32(body[off:]))
		off += 4
		b.Red[i].Seq = seq
		if err := read(&b.Red[i].Samples, rn); err != nil {
			return nil, err
		}
	}
	return b, nil
}
