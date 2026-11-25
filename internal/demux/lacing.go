package demux

import (
	"errors"

	"mediaproc/internal/ebml"
)

// parseLacedFrames splits a block payload after its three-byte block header.
// lacing is the raw flags & LacingMask value.
func parseLacedFrames(payload []byte, pos int, lacing byte) ([][]byte, error) {
	if pos > len(payload) {
		return nil, errors.New("lacing header starts beyond payload")
	}
	switch lacing {
	case ebml.LacingNone:
		return [][]byte{payload[pos:]}, nil
	case ebml.LacingXiph:
		return parseXiphLacing(payload, pos)
	case ebml.LacingFixed:
		return parseFixedLacing(payload, pos)
	case ebml.LacingEBML:
		return parseEBMLLacing(payload, pos)
	default:
		return nil, errors.New("unknown lacing flags")
	}
}

func parseXiphLacing(payload []byte, pos int) ([][]byte, error) {
	if pos >= len(payload) {
		return nil, errors.New("missing Xiph frame count")
	}
	count := int(payload[pos]) + 1
	pos++
	if count <= 0 || count > 256 {
		return nil, errors.New("invalid Xiph frame count")
	}
	sizes := make([]int, count)
	for i := 0; i < count-1; i++ {
		size := 0
		for {
			if pos >= len(payload) {
				return nil, errors.New("truncated Xiph size coding")
			}
			b := payload[pos]
			pos++
			size += int(b)
			if b != 255 {
				break
			}
			if size > len(payload) {
				return nil, errors.New("Xiph size overflow")
			}
		}
		sizes[i] = size
	}

	dataStart := pos
	off := 0
	frames := make([][]byte, 0, count)
	for i := 0; i < count-1; i++ {
		if sizes[i] < 0 || off+sizes[i] > len(payload)-dataStart {
			return nil, errors.New("Xiph frame size exceeds payload")
		}
		frames = append(frames, payload[dataStart+off:dataStart+off+sizes[i]])
		off += sizes[i]
	}
	last := len(payload) - dataStart - off
	if last < 0 {
		return nil, errors.New("negative final Xiph frame size")
	}
	frames = append(frames, payload[dataStart+off:])
	return frames, nil
}

func parseFixedLacing(payload []byte, pos int) ([][]byte, error) {
	if pos >= len(payload) {
		return nil, errors.New("missing fixed-lacing frame count")
	}
	count := int(payload[pos]) + 1
	pos++
	remaining := len(payload) - pos
	if count <= 0 || remaining < 0 || remaining%count != 0 {
		return nil, errors.New("fixed-lacing payload is not evenly sized")
	}
	size := remaining / count
	frames := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		start := pos + i*size
		frames = append(frames, payload[start:start+size])
	}
	return frames, nil
}

// parseEBMLLacing accepts the common form where frame edge sizes are EBML
// VINTs. Invalid/truncated codings are rejected for recovery.
func parseEBMLLacing(payload []byte, pos int) ([][]byte, error) {
	if pos >= len(payload) {
		return nil, errors.New("missing EBML-lacing frame count")
	}
	count := int(payload[pos]) + 1
	pos++
	if count <= 0 || count > 256 {
		return nil, errors.New("invalid EBML lacing count")
	}
	sizes := make([]int, count-1)
	for i := 0; i < count-1; i++ {
		v, n, err := ebml.DecodeVINT(payload[pos:], false)
		if err != nil {
			return nil, errors.New("truncated EBML lacing size")
		}
		sizes[i] = int(v.Value)
		pos += n
	}
	dataStart := pos
	off := 0
	frames := make([][]byte, 0, count)
	for i := 0; i < count-1; i++ {
		if off+sizes[i] > len(payload)-dataStart {
			return nil, errors.New("EBML lacing frame exceeds payload")
		}
		frames = append(frames, payload[dataStart+off:dataStart+off+sizes[i]])
		off += sizes[i]
	}
	if len(payload)-dataStart-off < 0 {
		return nil, errors.New("negative final EBML lacing size")
	}
	frames = append(frames, payload[dataStart+off:])
	return frames, nil
}
