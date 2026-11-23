// Package fixtures builds the deterministic sample media that the demo and
// tests consume. Everything is generated in memory or into a caller-supplied
// directory; no external assets are referenced.
//
// The builders emit real ISOBMFF and MXF byte streams at the subset of the
// formats mediasync reads, plus deliberately damaged variants used to exercise
// error propagation.
package fixtures

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

// Limits respected by every builder: no single fixture exceeds 8 MiB and the
// demo refuses to build anything larger than maxFixtureBytes.
const (
	maxFixtureBytes = 8 << 20
	// MaxBlockBytes mirrors the checkpoint block cap (64 MiB) minus headroom.
	MaxBlockBytes = 64 << 20
)

// Options controls generated media sizes and metadata quirks.
type Options struct {
	Seed int64
	// VideoFrames is the number of coded video samples.
	VideoFrames int
	// AudioSamples is the number of coded audio samples.
	AudioSamples int
	// FPS numerator/denominator of the video frame rate (e.g. 30000/1001).
	FPSNum, FPSDen int
	// SampleRate of the audio track.
	SampleRate int
	// Width/Height are the coded video dimensions.
	Width, Height int
	// Rotation written into the tkhd matrix (0, 90, 180, 270).
	Rotation int
	// WithColour emits a colr box with the given codes.
	ColourPrimaries, ColourTransfer, ColourMatrix uint16
	// UnknownColour uses codes outside the known set.
	UnknownColour bool
	// CropPixels emits a clap box cropping this many pixels from every edge.
	CropPixels int
	// ClapCleanW/ClapCleanH override the clap clean aperture, used to build
	// deliberately out-of-bounds crops.
	ClapCleanW, ClapCleanH int
	// VideoTrackID/AudioTrackID override the default track IDs.
	VideoTrackID, AudioTrackID uint32
}

// DefaultOptions returns a compact, deterministic 2-track fixture.
func DefaultOptions() Options {
	return Options{
		Seed:            41,
		VideoFrames:     30,
		AudioSamples:    48,
		FPSNum:          30000,
		FPSDen:          1001,
		SampleRate:      48000,
		Width:           1920,
		Height:          1080,
		Rotation:        90,
		ColourPrimaries: 1, ColourTransfer: 1, ColourMatrix: 1,
		VideoTrackID: 1,
		AudioTrackID: 2,
	}
}

func (o Options) validated() (Options, error) {
	if o.VideoFrames < 0 || o.AudioSamples < 0 {
		return o, errors.New("fixtures: negative sample counts")
	}
	if o.FPSNum <= 0 || o.FPSDen <= 0 {
		return o, errors.New("fixtures: invalid frame rate")
	}
	if o.SampleRate <= 0 {
		return o, errors.New("fixtures: invalid sample rate")
	}
	if o.Width <= 0 || o.Height <= 0 {
		return o, errors.New("fixtures: invalid dimensions")
	}
	switch o.Rotation {
	case 0, 90, 180, 270:
	default:
		return o, fmt.Errorf("fixtures: unsupported rotation %d", o.Rotation)
	}
	if o.CropPixels < 0 {
		return o, errors.New("fixtures: negative crop")
	}
	if o.VideoTrackID == 0 || o.AudioTrackID == 0 || o.VideoTrackID == o.AudioTrackID {
		return o, errors.New("fixtures: track IDs must be distinct and non-zero")
	}
	return o, nil
}

// Payload returns deterministic coded bytes for sample (track, index). A short
// NAL-like start code makes the data look like coded essence without claiming
// to be decodable.
func Payload(seed int64, track uint32, index, size int) []byte {
	if size <= 0 {
		return nil
	}
	if size > MaxBlockBytes {
		size = MaxBlockBytes
	}
	r := rand.New(rand.NewSource(seed*1000003 + int64(track)*7919 + int64(index)))
	buf := make([]byte, size)
	r.Read(buf)
	// Write a recognisable 4-byte prefix so truncation is visible in hexdumps.
	if size >= 4 {
		copy(buf, []byte{0x00, 0x00, 0x00, 0x01})
	}
	return buf
}

// sampleSize dithers sizes deterministically so byte totals are not uniform.
func sampleSize(seed int64, track uint32, index, base, spread int) int {
	r := rand.New(rand.NewSource(seed*31 + int64(track)*17 + int64(index)))
	return base + r.Intn(spread+1)
}

// HashSamples returns the SHA-256 over the ordered coded payloads, which is the
// identity the remuxer must preserve.
func HashSamples(payloads [][]byte) ([32]byte, error) {
	h := sha256.New()
	for i, p := range payloads {
		if _, err := h.Write(p); err != nil {
			return [32]byte{}, fmt.Errorf("fixtures: hash sample %d: %w", i, err)
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// WriteFile writes data under dir after rejecting path escapes.
func WriteFile(dir, name string, data []byte) (string, error) {
	if len(data) > maxFixtureBytes {
		return "", fmt.Errorf("fixtures: %s exceeds %d bytes", name, maxFixtureBytes)
	}
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, string(filepath.Separator)+"..") {
		return "", fmt.Errorf("fixtures: path escape rejected: %s", name)
	}
	path := filepath.Join(dir, clean)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// concat joins byte slices for fixed-layout payload fields.
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }
