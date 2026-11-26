package fixture

import (
	"encoding/binary"
	"math/rand"
	"os"

	"archivectl/internal/core"
)

// Cue is a compact generator cue/keyframe description.
type Cue struct {
	ID    uint32
	Frame int64
}

// EncodePCM creates deterministic little-endian PCM bytes.
func EncodePCM(seed int64, frames, channels, bits int) ([]byte, error) {
	const op = "fixture.EncodePCM"
	if frames < 0 || channels < 1 || channels > 8 {
		return nil, core.New(core.KindInvalidArgument, op, "invalid frames/channels")
	}
	switch bits {
	case 8, 16, 24, 32:
	default:
		return nil, core.Newf(core.KindUnsupported, op, "unsupported bits %d", bits)
	}
	bytesPerSample := bits / 8
	n := int64(frames) * int64(channels) * int64(bytesPerSample)
	if err := core.CheckAllocation(op, n); err != nil {
		return nil, err
	}
	pcm := make([]byte, n)
	rng := rand.New(rand.NewSource(seed))
	for frame := 0; frame < frames; frame++ {
		envelope := int64(1)
		if frame%(4096) < 2048 {
			envelope = 1
		} else {
			envelope = -1
		}
		for ch := 0; ch < channels; ch++ {
			idx := (frame*channels + ch) * bytesPerSample
			// Deterministic but non-silent content; corruption changes CRCs.
			v := int64(rng.Intn(1<<20)) + int64(frame*7+ch*101)
			v = (v%1_900_000 - 950_000) * envelope
			switch bits {
			case 8:
				pcm[idx] = byte((v >> 8) + 128)
			case 16:
				binary.LittleEndian.PutUint16(pcm[idx:idx+2], uint16(int16(v>>8)))
			case 24:
				x := int32(v)
				pcm[idx] = byte(x)
				pcm[idx+1] = byte(x >> 8)
				pcm[idx+2] = byte(x >> 16)
			case 32:
				binary.LittleEndian.PutUint32(pcm[idx:idx+4], uint32(int32(v)))
			}
		}
	}
	return pcm, nil
}

type writerOptions struct {
	SampleRate     int
	Channels       int
	Bits           int
	Cues           []Cue
	IncludeUnknown bool
	OmitUnknownPad bool
}

// WriteWAV writes a PCM RIFF/WAVE file with optional cue and unknown chunks.
func WriteWAV(path string, pcm []byte, opt writerOptions) error {
	const op = "fixture.WriteWAV"
	frameBytes := opt.Channels * (opt.Bits / 8)
	if frameBytes <= 0 || len(pcm)%frameBytes != 0 {
		return core.Newf(core.KindAlignment, op, "PCM length %d not aligned to frame bytes %d", len(pcm), frameBytes)
	}
	cueBody := encodeCueBody(opt.Cues)
	unknownBody := []byte{0x41, 0x52, 0x43} // "ARC", intentionally odd.

	riffSize := uint32(4 + 8 + 16) // WAVE + fmt header/body
	if len(cueBody) > 0 {
		riffSize += uint32(8 + len(cueBody) + len(cueBody)%2)
	}
	if opt.IncludeUnknown {
		pad := 1
		if !opt.OmitUnknownPad {
			riffSize += uint32(8 + len(unknownBody) + pad)
		} else {
			riffSize += uint32(8 + len(unknownBody))
		}
	}
	riffSize += uint32(8 + len(pcm) + len(pcm)%2)

	f, err := os.Create(path)
	if err != nil {
		return core.Wrap(core.KindIO, op, path, err)
	}
	defer f.Close()
	le := func(v any) error { return binary.Write(f, binary.LittleEndian, v) }
	if _, err := f.WriteString("RIFF"); err != nil {
		return err
	}
	if err := le(riffSize); err != nil {
		return err
	}
	if _, err := f.WriteString("WAVE"); err != nil {
		return err
	}
	if _, err := f.WriteString("fmt "); err != nil {
		return err
	}
	if err := le(uint32(16)); err != nil {
		return err
	}
	blockAlign := uint16(frameBytes)
	byteRate := uint32(opt.SampleRate * frameBytes)
	for _, v := range []any{uint16(1), uint16(opt.Channels), uint32(opt.SampleRate), byteRate, blockAlign, uint16(opt.Bits)} {
		if err := le(v); err != nil {
			return err
		}
	}
	if len(cueBody) > 0 {
		if _, err := f.WriteString("cue "); err != nil {
			return err
		}
		if err := le(uint32(len(cueBody))); err != nil {
			return err
		}
		if _, err := f.Write(cueBody); err != nil {
			return err
		}
		if len(cueBody)%2 == 1 {
			if _, err := f.Write([]byte{0}); err != nil {
				return err
			}
		}
	}
	if opt.IncludeUnknown {
		if _, err := f.WriteString("JUNK"); err != nil {
			return err
		}
		if err := le(uint32(len(unknownBody))); err != nil {
			return err
		}
		if _, err := f.Write(unknownBody); err != nil {
			return err
		}
		if len(unknownBody)%2 == 1 && !opt.OmitUnknownPad {
			if _, err := f.Write([]byte{0}); err != nil {
				return err
			}
		}
	}
	if _, err := f.WriteString("data"); err != nil {
		return err
	}
	if err := le(uint32(len(pcm))); err != nil {
		return err
	}
	if _, err := f.Write(pcm); err != nil {
		return err
	}
	if len(pcm)%2 == 1 {
		_, _ = f.Write([]byte{0})
	}
	return f.Sync()
}

func encodeCueBody(cues []Cue) []byte {
	if len(cues) == 0 {
		return nil
	}
	b := make([]byte, 4+24*len(cues))
	binary.LittleEndian.PutUint32(b[0:4], uint32(len(cues)))
	for i, c := range cues {
		off := 4 + i*24
		binary.LittleEndian.PutUint32(b[off:off+4], c.ID)
		binary.LittleEndian.PutUint32(b[off+4:off+8], uint32(c.Frame)) // play position
		copy(b[off+8:off+12], "data")
		// chunk start and block start are zero for uncompressed PCM.
		binary.LittleEndian.PutUint32(b[off+20:off+24], uint32(c.Frame))
	}
	return b
}
