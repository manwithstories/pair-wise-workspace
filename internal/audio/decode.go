package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"mediad/internal/probe"
)

// The sample batches carry raw little-endian 16-bit PCM in the mdat payload
// (an ipcm-style payload). Real compressed codecs cannot be decoded without a
// codec library, so they are reported as undecodable and preserved as gaps.

// ErrNoMedia marks a segment whose payload cannot be read.
var ErrNoMedia = errors.New("audio: no decodable media payload")

// DecodePCM extracts interleaved float64 samples from an fMP4 media segment
// whose mdat holds 16-bit little-endian PCM. The probe result must have a
// determined timeline and must not be truncated.
func DecodePCM(data []byte, info *probe.MediaInfo, rate, channels int) (Source, error) {
	if rate <= 0 || channels <= 0 {
		return Source{}, ErrInvalidRate
	}
	if info == nil {
		return Source{}, fmt.Errorf("%w: missing probe result", ErrNoMedia)
	}
	if !info.Determined {
		return Source{}, fmt.Errorf("%w: timeline undetermined", ErrNoMedia)
	}
	if info.Truncated {
		return Source{}, fmt.Errorf("%w: segment truncated", ErrNoMedia)
	}
	payload, err := findMdatPayload(data)
	if err != nil {
		return Source{}, err
	}
	var expected int64
	for _, frag := range info.Fragments {
		if !frag.Determined || !frag.HasMdat {
			continue
		}
		for _, s := range frag.Samples {
			expected += int64(s.Size)
		}
	}
	if expected <= 0 {
		return Source{}, fmt.Errorf("%w: no samples", ErrNoMedia)
	}
	if int64(len(payload)) < expected {
		return Source{}, fmt.Errorf("%w: mdat has %d of %d bytes", ErrNoMedia, len(payload), expected)
	}
	frameBytes := int64(channels * 2)
	if expected%frameBytes != 0 {
		return Source{}, fmt.Errorf("%w: payload is not whole frames", ErrNoMedia)
	}
	frames := int(expected / frameBytes)
	samples := make([]float64, frames*channels)
	for i := 0; i < frames; i++ {
		for ch := 0; ch < channels; ch++ {
			off := (int64(i)*int64(channels) + int64(ch)) * 2
			v := int16(binary.LittleEndian.Uint16(payload[off:]))
			samples[i*channels+ch] = float64(v) / math.MaxInt16
		}
	}
	return Source{
		URI: info.URI, StartTicks: info.StartTicks,
		DurationTicks: info.EndTicks - info.StartTicks,
		SampleRate:    rate, Channels: channels, Samples: samples, Decodable: true,
	}, nil
}

// findMdatPayload returns the payload of the first complete mdat box.
func findMdatPayload(data []byte) ([]byte, error) {
	off := 0
	for off+8 <= len(data) {
		size := int64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		header := int64(8)
		if size == 1 {
			if off+16 > len(data) {
				return nil, fmt.Errorf("%w: large header truncated", ErrNoMedia)
			}
			size = int64(binary.BigEndian.Uint64(data[off+8:]))
			header = 16
		} else if size == 0 {
			size = int64(len(data) - off)
		}
		if size < header || int64(off)+size > int64(len(data)) {
			return nil, fmt.Errorf("%w: box %q overflows", ErrNoMedia, typ)
		}
		if typ == "mdat" {
			return data[off+int(header) : off+int(size)], nil
		}
		off += int(size)
	}
	return nil, fmt.Errorf("%w: no mdat box", ErrNoMedia)
}

// EncodePCM16 serialises interleaved samples as little-endian 16-bit PCM,
// clamping out-of-range values. This is the canonical delivery byte form used
// for per-segment hashes and independent verification.
func EncodePCM16(samples []float64) []byte {
	out := make([]byte, len(samples)*2)
	for i, v := range samples {
		f := math.Round(v * math.MaxInt16)
		if f > math.MaxInt16 {
			f = math.MaxInt16
		} else if f < math.MinInt16 {
			f = math.MinInt16
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(f)))
	}
	return out
}
