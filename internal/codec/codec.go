// Package codec is the AdapterV1 parsing/codec boundary.
package codec

import (
	"encoding/binary"
	"math"

	"mediad/internal/fault"
	"mediad/internal/manifest"
)

const (
	// AdapterVersion is the only codec boundary generation accepted here.
	AdapterVersion = 1
	// SupportedSpecVersion identifies fixtures/materials understood by v1.
	SupportedSpecVersion = "mediad-source-v1"
)

// AdapterV1 rejects unknown versions/codecs with located faults.
type AdapterV1 struct {
	DefaultAudioChannels int
}

// NewV1 constructs the version-one adapter.
func NewV1() *AdapterV1 {
	return &AdapterV1{DefaultAudioChannels: 2}
}

// Open validates the source descriptor and returns a ready session.
func (a *AdapterV1) Open(desc manifest.CodecDescriptor) (*Session, error) {
	if desc.AdapterVersion != 0 && desc.AdapterVersion != AdapterVersion {
		return nil, fault.New(fault.UnknownVersion, sourcePath(desc), 0,
			"unknown adapter version %d; want %d", desc.AdapterVersion, AdapterVersion)
	}
	if desc.SpecVersion != SupportedSpecVersion {
		return nil, fault.New(fault.UnknownVersion, sourcePath(desc), 0,
			"unknown source spec version %q; want %q", desc.SpecVersion, SupportedSpecVersion)
	}
	if desc.Container != "fmp4" {
		return nil, fault.New(fault.UnsupportedCodec, sourcePath(desc), 0,
			"container %q unsupported in adapter v1; want fmp4", desc.Container)
	}
	video := normalizeVideoCodec(desc.VideoCodec)
	if video != "h264" {
		return nil, fault.New(fault.UnsupportedCodec, sourcePath(desc), 0,
			"video codec %q unsupported in adapter v1; want H.264", desc.VideoCodec)
	}
	audio := normalizeAudioCodec(desc.AudioCodec)
	if audio != "pcm_s16le" {
		return nil, fault.New(fault.UnsupportedCodec, sourcePath(desc), 0,
			"audio codec %q unsupported in adapter v1; want PCM16LE", desc.AudioCodec)
	}
	return &Session{adapter: a, Video: video, Audio: audio}, nil
}

// Session is an opened codec boundary.
type Session struct {
	adapter *AdapterV1
	Video   string
	Audio   string
}

// DecodePCM16LE converts little-endian interleaved bytes to int16 samples.
func (s *Session) DecodePCM16LE(path string, data []byte) ([]int16, error) {
	if len(data)%2 != 0 {
		return nil, fault.New(fault.MdatCorrupt, path, int64(len(data)), "PCM16LE payload has odd length")
	}
	out := make([]int16, len(data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(data[2*i : 2*i+2]))
	}
	return out, nil
}

// EncodePCM16LE converts signed samples to little-endian bytes.
func (s *Session) EncodePCM16LE(samples []int16) []byte {
	out := make([]byte, 2*len(samples))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

// RoundtripBlock verifies exact PCM16LE encode/decode identity for one block.
func (s *Session) RoundtripBlock(block []int16) (bool, error) {
	encoded := s.EncodePCM16LE(block)
	decoded, err := s.DecodePCM16LE("roundtrip", encoded)
	if err != nil {
		return false, err
	}
	if len(decoded) != len(block) {
		return false, nil
	}
	for i := range block {
		if decoded[i] != block[i] {
			return false, nil
		}
	}
	return true, nil
}

// FloatFromPCM16 maps a sample to [-1, 1].
func FloatFromPCM16(v int16) float64 {
	if v == math.MinInt16 {
		return -1
	}
	return float64(v) / 32768.0
}

// PCM16FromFloat clamps and quantizes a float sample.
func PCM16FromFloat(v float64) int16 {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	if v >= 0 {
		return int16(math.Round(v * 32767))
	}
	return int16(math.Round(v * 32768))
}

func normalizeVideoCodec(in string) string {
	switch in {
	case "avc1", "avc3", "h264", "H264", "video/avc":
		return "h264"
	default:
		return in
	}
}

func normalizeAudioCodec(in string) string {
	switch in {
	case "ipcm", "pcm ", "lpcm", "pcm_s16le", "PCM16LE", "audio/pcm-s16le":
		return "pcm_s16le"
	default:
		return in
	}
}

func sourcePath(d manifest.CodecDescriptor) string {
	return d.Path
}
