// Package fixture generates deterministic minimal MP4 files for testing:
// video+audio tracks, B-frame timestamps, edit lists, keyframe intervals,
// 32-bit timestamp wrap, and fault injection.
package fixture

import (
	"bytes"
	"encoding/binary"

	mp4 "github.com/abema/go-mp4"
)

// prng is a deterministic splitmix64 generator.
type prng struct {
	s    uint64
	last uint64
}

func newPRNG(seed int64) *prng { return &prng{s: uint64(seed)} }

func (p *prng) next() uint64 {
	p.s += 0x9E3779B97F4A7C15
	z := p.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// rangeInt returns a deterministic value in [lo, hi].
func (p *prng) rangeInt(lo, hi int) int {
	return lo + int(p.next()%uint64(hi-lo+1))
}

// bytes fills dst deterministically.
func (p *prng) bytes(dst []byte) {
	for i := range dst {
		if i%8 == 0 {
			p.last = p.next()
		}
		dst[i] = byte(p.last >> ((i % 8) * 8))
	}
}

// boxWriter assembles MP4 boxes; headers come from go-mp4's codec.
type boxWriter struct {
	buf bytes.Buffer
}

// box appends one box with the given payload.
func (w *boxWriter) box(typ string, payload []byte) {
	hdr := mp4.EncodeBoxInfo(&mp4.BoxInfo{
		Type:       mp4.StrToBoxType(typ),
		Size:       uint64(8 + len(payload)),
		HeaderSize: 8,
	})
	w.buf.Write(hdr)
	w.buf.Write(payload)
}

// fullBox appends a full box (version + flags) with the given payload.
func (w *boxWriter) fullBox(typ string, version byte, flags uint32, payload []byte) {
	fh := make([]byte, 4)
	fh[0] = version
	fh[1] = byte(flags >> 16)
	fh[2] = byte(flags >> 8)
	fh[3] = byte(flags)
	w.box(typ, append(fh, payload...))
}

// container appends a container box built by fn.
func (w *boxWriter) container(typ string, fn func(w *boxWriter)) {
	sub := &boxWriter{}
	fn(sub)
	w.box(typ, sub.buf.Bytes())
}

// --- little field helpers (big-endian on the wire) ---

func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u24(v uint32) []byte { return []byte{byte(v >> 16), byte(v >> 8), byte(v)} }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
