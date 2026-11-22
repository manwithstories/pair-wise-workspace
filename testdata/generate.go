// Package testdata generates reproducible media fixtures entirely in Go:
// hand-built MP4 boxes for the documented subset, KLV framing for the MXF
// OP1a subset, deterministic sample payloads, and controlled corruptions.
// No fixture references material outside the repository.
package testdata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"

	"mediarun/internal/ingest"
)

// FixtureSpec configures one generated container.
type FixtureSpec struct {
	Path         string
	Format       string // "mp4" | "mxf"
	VideoSamples int
	VideoScale   int64 // MP4 timescale / MXF edit rate ticks per second
	FrameDur     int64 // ticks per video sample
	Width        uint32
	Height       uint32
	AudioSamples int
	AudioRate    int64
	// TargetBitrate, when > 0, sizes payloads so total encoded bytes equal
	// bitrate * media_duration / 8 exactly, making the reported bitrate match
	// the target within floating-point rounding. Zero keeps geometry sizing.
	TargetBitrate int64
	// Injection, when non-empty, corrupts the file deterministically:
	// "truncated-box" cuts an MP4 box short; "truncated-klv" cuts an MXF
	// essence payload short; "bad-magic" overwrites the leading bytes.
	Injection string
}

// SampleByte returns the deterministic payload byte for a sample, so the
// same input+config always yields the same output hash without storing any
// external material.
func SampleByte(trackIndex, sampleIndex int64) byte {
	return byte((trackIndex*31 + sampleIndex*17) & 0xff)
}

// Write builds the fixture described by spec and writes it atomically-ish
// (caller's directory is inside the sandboxed work dir).
func Write(spec FixtureSpec) error {
	if spec.VideoScale == 0 {
		spec.VideoScale = 30000
	}
	if spec.FrameDur == 0 {
		spec.FrameDur = 1001
	}
	if spec.AudioRate == 0 {
		spec.AudioRate = 48000
	}
	var raw []byte
	var err error
	switch spec.Format {
	case "mp4":
		raw, err = buildMP4(spec)
	case "mxf":
		raw, err = buildMXF(spec)
	default:
		return errors.New("unknown fixture format: " + spec.Format)
	}
	if err != nil {
		return err
	}
	raw = inject(spec, raw)
	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(spec.Path, raw, 0o644)
}

// box assembles one MP4 box: 32-bit size + type + payload.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// fullbox prefixes a version/flags FullBox header.
func fullbox(version byte, flags uint32) []byte {
	h := make([]byte, 4)
	h[0] = version
	h[1] = byte(flags >> 16)
	h[2] = byte(flags >> 8)
	h[3] = byte(flags)
	return h
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// cat concatenates byte slices (Go has no + operator on []byte).
func cat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// videoPayload / audioPayload produce deterministic, bounded sample bytes.
func videoPayload(i int64, w, h uint32) []byte {
	size := int64(w) * int64(h) / 40 // tiny planar stand-in, far under 4 MiB
	if size < 16 {
		size = 16
	}
	return sizedPayload(0, i, size)
}

func audioPayload(i int64) []byte {
	return sizedPayload(1, i, 256) // 128 stereo samples @16-bit
}

// sizedPayload builds a deterministic buffer of exactly size bytes.
func sizedPayload(track, i, size int64) []byte {
	if size < 1 {
		size = 1
	}
	p := make([]byte, size)
	base := SampleByte(track, i)
	for k := range p {
		p[k] = base ^ byte(k)
	}
	return p
}

// SizedPayloads returns per-sample payloads whose total byte count equals
// targetTotal exactly: an even base size per sample with the integer
// remainder spread over the first samples. All sizes stay well under the
// 4 MiB block ceiling for the demo workloads.
func SizedPayloads(track, count int, targetTotal int64) [][]byte {
	if count <= 0 {
		return nil
	}
	base := targetTotal / int64(count)
	rem := targetTotal - base*int64(count)
	out := make([][]byte, count)
	for i := 0; i < count; i++ {
		size := base
		if int64(i) < rem {
			size++
		}
		out[i] = sizedPayload(int64(track), int64(i), size)
	}
	return out
}

// TotalInputSamples reports how many samples a spec produces across tracks.
func TotalInputSamples(spec FixtureSpec) int {
	return spec.VideoSamples + spec.AudioSamples
}

var _ = bytes.Equal
var _ = ingest.MaxBlockBytes
