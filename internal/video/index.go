// Package video builds a keyframe index over a synthetic VFR frame
// stream and verifies random-access seeks by decoding (CRC-checking)
// the target keyframe. Index positions come from frame headers only,
// so a corrupt frame never shifts subsequent entries.
package video

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"

	"archiver/internal/media"
)

const frameHeaderLen = 8 + 1 + 4 // pts + flags + payload length

// Frame is one parsed video frame. PTS keeps the original variable
// frame rate timing.
type Frame struct {
	PTS  media.TimeUs
	Key  bool
	Off  int64 // offset of the frame header in the stream
	Raw  []byte
	Data []byte // payload
}

// EncodeFrame serializes one frame: header, payload, CRC-32 of both.
func EncodeFrame(pts media.TimeUs, key bool, payload []byte) []byte {
	out := make([]byte, frameHeaderLen+len(payload)+4)
	binary.BigEndian.PutUint64(out[0:8], uint64(pts))
	if key {
		out[8] = 1
	}
	binary.BigEndian.PutUint32(out[9:13], uint32(len(payload)))
	copy(out[frameHeaderLen:], payload)
	crc := crc32.ChecksumIEEE(out[:frameHeaderLen+len(payload)])
	binary.BigEndian.PutUint32(out[frameHeaderLen+len(payload):], crc)
	return out
}

// ParseStream decodes frame headers (and payloads) from a bitstream.
func ParseStream(buf []byte) ([]Frame, error) {
	var frames []Frame
	for off := 0; off < len(buf); {
		if len(buf)-off < frameHeaderLen+4 {
			return nil, fmt.Errorf("truncated frame at offset %d", off)
		}
		n := int(binary.BigEndian.Uint32(buf[off+9 : off+13]))
		end := off + frameHeaderLen + n + 4
		if end > len(buf) {
			return nil, fmt.Errorf("truncated frame payload at offset %d", off)
		}
		raw := buf[off:end]
		frames = append(frames, Frame{
			PTS:  media.TimeUs(binary.BigEndian.Uint64(raw[0:8])),
			Key:  raw[8]&1 != 0,
			Off:  int64(off),
			Raw:  raw,
			Data: raw[frameHeaderLen : frameHeaderLen+n],
		})
		off = end
	}
	return frames, nil
}

// decode verifies a frame's integrity without touching any other frame.
func decode(f Frame) bool {
	want := binary.BigEndian.Uint32(f.Raw[len(f.Raw)-4:])
	return crc32.ChecksumIEEE(f.Raw[:len(f.Raw)-4]) == want
}

// Index maps keyframe PTS to stream positions.
type Index struct {
	frames []Frame
	keys   []int // indexes into frames, PTS ascending
}

// Build indexes frames by header only; corrupt payloads do not affect
// the positions of subsequent entries.
func Build(frames []Frame) *Index {
	ix := &Index{frames: frames}
	for i, f := range frames {
		if f.Key {
			ix.keys = append(ix.keys, i)
		}
	}
	sort.Slice(ix.keys, func(a, b int) bool {
		return frames[ix.keys[a]].PTS < frames[ix.keys[b]].PTS
	})
	return ix
}

// Keyframes returns the indexed keyframe timestamps.
func (ix *Index) Keyframes() []media.TimeUs {
	out := make([]media.TimeUs, len(ix.keys))
	for i, k := range ix.keys {
		out[i] = ix.frames[k].PTS
	}
	return out
}

// Seek locates the last keyframe at or before target, then decodes
// forward to the target frame. It reports whether the located keyframe
// and the target frame both decode cleanly, plus the number of bad
// frames encountered on the way (which never shift the index).
func (ix *Index) Seek(target media.TimeUs) (hit bool, badFrames int) {
	if len(ix.keys) == 0 {
		return false, 0
	}
	ki := sort.Search(len(ix.keys), func(i int) bool {
		return ix.frames[ix.keys[i]].PTS > target
	}) - 1
	if ki < 0 {
		return false, 0
	}
	start := ix.keys[ki]
	if !decode(ix.frames[start]) {
		badFrames++
	}
	ti := start
	for ti < len(ix.frames) && ix.frames[ti].PTS < target {
		ti++
	}
	if ti >= len(ix.frames) || ix.frames[ti].PTS != target {
		return false, badFrames
	}
	for i := start + 1; i <= ti; i++ {
		if !decode(ix.frames[i]) {
			badFrames++
		}
	}
	return decode(ix.frames[ti]), badFrames
}
