// Package fixture generates deterministic medpipe containers entirely in
// memory: no external files, devices or network access are involved. It also
// applies the three corruption modes used by the demo and tests.
package fixture

import (
	"bytes"
	"encoding/binary"
	"encoding/json"

	"medpipe/internal/model"
)

// Options controls one generated container.
type Options struct {
	Seed          int64
	Frames        int
	AudioPerVideo int // audio blocks per video frame (0 disables audio)
	Width         int
	Height        int
	FrameStepNS   int64
	AudioStepNS   int64
}

// Defaults fills unset options with the demo values.
func (o *Options) Defaults() {
	if o.Frames <= 0 {
		o.Frames = 240
	}
	if o.AudioPerVideo == 0 {
		o.AudioPerVideo = 1
	}
	if o.Width == 0 {
		o.Width = 64
	}
	if o.Height == 0 {
		o.Height = 64
	}
	if o.FrameStepNS == 0 {
		o.FrameStepNS = 40_000_000 // 25 fps
	}
	if o.AudioStepNS == 0 {
		o.AudioStepNS = 80_000_000 // 12.5 blocks/s at 8 kHz
	}
}

// Generate writes a fully checksummed container and returns its bytes.
func Generate(o Options) ([]byte, error) {
	o.Defaults()
	audioCount := o.Frames * o.AudioPerVideo
	var buf bytes.Buffer
	if err := model.WriteHeader(&buf, uint32(o.Frames+audioCount), 0); err != nil {
		return nil, err
	}
	// Deterministic LCG so every run with the same seed matches exactly.
	state := uint64(o.Seed)
	next := func() uint64 {
		state = state*6364136223846793005 + 1442695040888963407
		return state
	}

	for i := 0; i < o.Frames; i++ {
		meta := model.VideoMeta{
			Width: o.Width, Height: o.Height, Color: "yuv420",
			Keyframe: i%30 == 0, FrameNum: uint32(i),
		}
		raw, _ := json.Marshal(meta)
		payload := make([]byte, 64)
		binary.BigEndian.PutUint32(payload, uint32(i%12)) // periodic duplicate frames
		binary.BigEndian.PutUint32(payload[4:], uint32(next()))
		flags := uint16(0)
		if meta.Keyframe {
			flags = model.FlagKeyframe
		}
		if _, err := model.WriteRecord(&buf, model.KindVideo, flags, uint32(i),
			int64(i)*o.FrameStepNS, raw, payload); err != nil {
			return nil, err
		}
	}
	for i := 0; i < audioCount; i++ {
		meta := model.AudioMeta{SampleRate: 8000, Channels: 1, Samples: 160}
		raw, _ := json.Marshal(meta)
		payload := make([]byte, 320)
		// Every 8th block carries a tone; the rest are digital silence.
		if i%8 == 0 {
			for s := 0; s < 160; s++ {
				binary.LittleEndian.PutUint16(payload[2*s:], uint16(int16(2000)))
			}
		}
		if _, err := model.WriteRecord(&buf, model.KindAudio, 0, uint32(i),
			int64(i)*o.AudioStepNS, raw, payload); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// CorruptFrame flips payload bytes inside the selected video frame so its
// checksum fails. It returns the byte offset of the damaged record.
func CorruptFrame(raw []byte, frameSeq uint32) (int64, error) {
	off, err := frameOffset(raw, frameSeq)
	if err != nil {
		return 0, err
	}
	// Flip bytes inside the payload region (after the 4-byte size prefix and
	// fixed 24-byte record header, well before the trailing checksum).
	raw[off+28] ^= 0xA5
	raw[off+29] ^= 0x5A
	return off, nil
}

// Truncate removes tail bytes to simulate an interrupted final write.
func Truncate(raw []byte, bytesToDrop int) []byte {
	if bytesToDrop >= len(raw) {
		return raw[:model.HeaderSize]
	}
	return raw[:len(raw)-bytesToDrop]
}

// frameOffset walks record size fields to locate a video frame's start.
func frameOffset(raw []byte, want uint32) (int64, error) {
	off := int64(model.HeaderSize)
	for int(off)+4 <= len(raw) {
		size := int64(binary.BigEndian.Uint32(raw[off : off+4]))
		if size < 24 || off+size > int64(len(raw)) {
			return 0, model.NewError(model.ErrTruncated, "cannot locate frame %d", want).At(off)
		}
		kind := binary.BigEndian.Uint16(raw[off+4 : off+6])
		seq := binary.BigEndian.Uint32(raw[off+8 : off+12])
		if kind == model.KindVideo && seq == want {
			return off, nil
		}
		off += size
	}
	return 0, model.NewError(model.ErrKind, "frame %d not found", want)
}
