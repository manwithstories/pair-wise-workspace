// Package mkv decodes Matroska block structures. It preserves each frame's
// absolute source byte range so the remuxer can prove that output sample bytes
// are a byte-for-byte substring of the archive master.
package mkv

import (
	"encoding/binary"
	"errors"
	"fmt"

	"archivecheck.local/archivecheck/internal/fault"
)

// Lacing modes.
const (
	LacingNone  = 0
	LacingXiph  = 1
	LacingFixed = 2
	LacingEBML  = 3
)

var (
	// ErrLacing marks malformed lacing headers or frame tables.
	ErrLacing = errors.New("malformed lacing")
	// ErrBlock marks a block payload shorter than its header.
	ErrBlock = errors.New("block shorter than header")
	// ErrEmptyFrame marks a zero-length frame, which MP4 cannot represent.
	ErrEmptyFrame = errors.New("empty frame")
)

// Frame is one decoded frame within a block together with its absolute byte
// range in the source file.
type Frame struct {
	// Data is the frame's bytes, copied out of the block payload.
	Data []byte
	// SourceOffset is the absolute offset of Data[0] in the input.
	SourceOffset int64
	// Size is len(Data).
	Size int
}

// Block is one SimpleBlock or Block/BlockGroup payload.
type Block struct {
	TrackNumber uint64
	// RelativeTimestamp is in Segment ticks and is added to the Cluster
	// timestamp to obtain the sample's absolute timestamp.
	RelativeTimestamp int16
	// Keyframe is the SimpleBlock keyframe flag, or BlockGroup's Keyframe
	// element. For audio tracks without flags it is true.
	Keyframe bool
	// Invisible, Discardable and Lacing mirror the Matroska flags.
	Invisible   bool
	Discardable bool
	Lacing      int
	// Frames holds the un-laced frames in block order.
	Frames []Frame
	// DurationTicks is the BlockGroup BlockDuration, 0 when absent.
	DurationTicks int64
	// SourceOffset is the absolute offset of the block payload.
	SourceOffset int64
}

// Block header flags.
const (
	flagKeyframe    = 0x80
	flagInvisible   = 0x08
	flagLacingMask  = 0x06
	flagDiscardable = 0x01
)

// ParseSimpleBlock decodes a SimpleBlock payload.
func ParseSimpleBlock(payload []byte, absoluteOffset int64) (Block, error) {
	return parseBlock(payload, absoluteOffset, true, true)
}

// ParseBlock decodes a (BlockGroup) Block payload. The keyframe status is not
// carried in this block form and must come from the BlockGroup Keyframe
// element; callers pass it via keyframe.
func ParseBlock(payload []byte, absoluteOffset int64, keyframe bool) (Block, error) {
	return parseBlock(payload, absoluteOffset, false, keyframe)
}

func parseBlock(payload []byte, absoluteOffset int64, simple bool, keyframe bool) (Block, error) {
	b := Block{SourceOffset: absoluteOffset}
	if len(payload) < 4 {
		return b, fault.AtElement(fault.StageDemux, absoluteOffset, "Block", ErrBlock)
	}
	track, n, err := readVint(payload, 0)
	if err != nil {
		return b, fault.AtElement(fault.StageDemux, absoluteOffset, "Block", err)
	}
	b.TrackNumber = track
	if n+3 > len(payload) {
		return b, fault.AtElement(fault.StageDemux, absoluteOffset+int64(n), "Block", ErrBlock)
	}
	b.RelativeTimestamp = int16(binary.BigEndian.Uint16(payload[n : n+2]))
	flags := payload[n+2]
	n += 3
	if simple {
		b.Keyframe = flags&flagKeyframe != 0
	} else {
		b.Keyframe = keyframe
	}
	b.Invisible = flags&flagInvisible != 0
	b.Discardable = flags&flagDiscardable != 0
	b.Lacing = int((flags & flagLacingMask) >> 1)
	frameData, frameOffsets, err := demuxFrames(payload[n:], b.Lacing, absoluteOffset+int64(n))
	if err != nil {
		return b, fault.AtElement(fault.StageDemux, absoluteOffset, "Block", err).OnTrack(track)
	}
	for i, d := range frameData {
		b.Frames = append(b.Frames, Frame{
			Data:         d,
			SourceOffset: frameOffsets[i],
			Size:         len(d),
		})
	}
	return b, nil
}

// demuxFrames splits the frame area according to the lacing mode. It returns
// frame slices that alias payload (no copy) plus their absolute offsets.
func demuxFrames(payload []byte, lacing int, base int64) ([][]byte, []int64, error) {
	switch lacing {
	case LacingNone:
		if len(payload) == 0 {
			return nil, nil, ErrEmptyFrame
		}
		return [][]byte{payload}, []int64{base}, nil
	case LacingFixed:
		if len(payload) < 1 {
			return nil, nil, fmt.Errorf("%w: missing frame count", ErrLacing)
		}
		count := int(payload[0]) + 1
		rest := payload[1:]
		if count == 0 || len(rest)%count != 0 {
			return nil, nil, fmt.Errorf("%w: %d bytes not divisible by %d frames", ErrLacing, len(rest), count)
		}
		size := len(rest) / count
		frames := make([][]byte, count)
		offsets := make([]int64, count)
		for i := 0; i < count; i++ {
			frames[i] = rest[i*size : (i+1)*size]
			offsets[i] = base + 1 + int64(i*size)
		}
		return frames, offsets, nil
	case LacingXiph:
		return demuxXiph(payload, base)
	case LacingEBML:
		return demuxEBML(payload, base)
	default:
		return nil, nil, fmt.Errorf("%w: unknown lacing %d", ErrLacing, lacing)
	}
}

func demuxXiph(payload []byte, base int64) ([][]byte, []int64, error) {
	if len(payload) < 1 {
		return nil, nil, fmt.Errorf("%w: missing frame count", ErrLacing)
	}
	count := int(payload[0]) + 1
	sizes := make([]int, count)
	pos := 1
	for i := 0; i < count-1; i++ {
		size := 0
		for {
			if pos >= len(payload) {
				return nil, nil, fmt.Errorf("%w: truncated Xiph lace sizes", ErrLacing)
			}
			v := payload[pos]
			pos++
			size += int(v)
			if v != 255 {
				break
			}
		}
		sizes[i] = size
	}
	remainder := len(payload) - pos
	var known int
	for i := 0; i < count-1; i++ {
		known += sizes[i]
	}
	if known > remainder {
		return nil, nil, fmt.Errorf("%w: Xiph frame sizes exceed payload", ErrLacing)
	}
	sizes[count-1] = remainder - known
	return splitFrames(payload, pos, sizes, base)
}

func demuxEBML(payload []byte, base int64) ([][]byte, []int64, error) {
	if len(payload) < 1 {
		return nil, nil, fmt.Errorf("%w: missing frame count", ErrLacing)
	}
	count := int(payload[0]) + 1
	sizes := make([]int, count)
	pos := 1
	first, n, err := readVint(payload, pos)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrLacing, err)
	}
	pos += n
	sizes[0] = int(first)
	for i := 1; i < count-1; i++ {
		delta, n, err := readSignedVint(payload, pos)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrLacing, err)
		}
		pos += n
		sizes[i] = sizes[i-1] + delta
		if sizes[i] < 0 {
			return nil, nil, fmt.Errorf("%w: negative EBML lace size", ErrLacing)
		}
	}
	remainder := len(payload) - pos
	var known int
	for i := 0; i < count-1; i++ {
		known += sizes[i]
	}
	if known > remainder {
		return nil, nil, fmt.Errorf("%w: EBML frame sizes exceed payload", ErrLacing)
	}
	sizes[count-1] = remainder - known
	return splitFrames(payload, pos, sizes, base)
}

func splitFrames(payload []byte, pos int, sizes []int, base int64) ([][]byte, []int64, error) {
	frames := make([][]byte, len(sizes))
	offsets := make([]int64, len(sizes))
	for i, size := range sizes {
		if size < 0 || pos+size > len(payload) {
			return nil, nil, fmt.Errorf("%w: frame %d exceeds payload", ErrLacing, i)
		}
		frames[i] = payload[pos : pos+size]
		offsets[i] = base + int64(pos)
		pos += size
	}
	return frames, offsets, nil
}

// readVint reads an EBML data vint (used for the track number).
func readVint(buf []byte, pos int) (uint64, int, error) {
	return readVintBuf(buf, pos)
}

// readSignedVint reads an EBML lace size delta (biased by 2^(7n-1)-1).
func readSignedVint(buf []byte, pos int) (int, int, error) {
	raw, n, err := readVintBuf(buf, pos)
	if err != nil {
		return 0, 0, err
	}
	bias := (1 << (7*uint(n) - 1)) - 1
	return int(raw) - bias, n, nil
}

func readVintBuf(buf []byte, pos int) (uint64, int, error) {
	if pos >= len(buf) {
		return 0, 0, fmt.Errorf("%w: missing vint", ErrLacing)
	}
	first := buf[pos]
	length := 0
	for i := 0; i < 8; i++ {
		if first&(0x80>>uint(i)) != 0 {
			length = i + 1
			break
		}
	}
	if length == 0 || pos+length > len(buf) {
		return 0, 0, fmt.Errorf("%w: invalid vint length", ErrLacing)
	}
	v := uint64(first) & (0xff >> uint(length))
	for i := 1; i < length; i++ {
		v = v<<8 | uint64(buf[pos+i])
	}
	return v, length, nil
}

// SyncFlagForAudio reports whether a codec's blocks are always sync samples.
// Matroska audio SimpleBlocks normally lack a keyframe flag; the remuxer needs
// every audio sample in the stss table regardless.
func SyncFlagForAudio(codec string) bool {
	switch codec {
	case "A_OPUS", "A_VORBIS", "A_AAC", "A_FLAC", "A_PCM/INT/LIT":
		return true
	default:
		return true
	}
}
