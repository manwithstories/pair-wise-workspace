package codec

import (
	"encoding/binary"
	"fmt"

	"example.com/medialab/archive/internal/clock"
)

// AACRate is the AAC RTP clock rate used by the archive profile (RFC 3640).
const AACRate clock.Rate = 90000

// AACConfig describes the out-of-band RFC 3640 parameters for one stream.
type AACConfig struct {
	SizeLength   int // AU-size field width in bits
	IndexLength  int // AU-index field width in bits
	SampleRate   int // decoder sample rate
	SamplesPerAU int // 1024 for plain AAC-LC, 2048 for SBR
}

// DefaultAACConfig matches the archive ingest profile.
func DefaultAACConfig() AACConfig {
	return AACConfig{SizeLength: 13, IndexLength: 3, SampleRate: 48000, SamplesPerAU: 1024}
}

// Validate checks the configuration before it is frozen into a checkpoint.
func (c AACConfig) Validate() error {
	if c.SizeLength <= 0 || c.SizeLength > 16 || c.IndexLength < 0 || c.IndexLength > 16 {
		return fmt.Errorf("codec: invalid AAC AU field widths %d/%d", c.SizeLength, c.IndexLength)
	}
	if c.SampleRate <= 0 || c.SamplesPerAU <= 0 {
		return fmt.Errorf("codec: invalid AAC samples %d@%d", c.SamplesPerAU, c.SampleRate)
	}
	return nil
}

// AACDurationUS parses the RFC 3640 AU-header section, verifies that the
// declared AU sizes exactly cover the payload and returns the media duration.
// A truncated or inconsistent header yields ErrUnderivable.
func (c AACConfig) AACDurationUS(payload []byte) (int64, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	if len(payload) < 2 {
		return 0, ErrShortPayload
	}
	bits := int(binary.BigEndian.Uint16(payload[:2]))
	if bits == 0 || bits%8 != 0 {
		return 0, fmt.Errorf("%w: AU header length %d bits", ErrUnderivable, bits)
	}
	headerBytes := bits / 8
	if 2+headerBytes > len(payload) {
		return 0, fmt.Errorf("%w: headers %d > payload %d", ErrUnderivable, headerBytes, len(payload))
	}
	stride := c.SizeLength + c.IndexLength
	if stride == 0 || bits%stride != 0 {
		return 0, fmt.Errorf("%w: %d header bits not divisible by stride %d", ErrUnderivable, bits, stride)
	}
	count := bits / stride
	// Read AU sizes; the width may not be byte aligned, so use a bit cursor.
	sizes := make([]int, count)
	cursor := 2
	bitPos := 0
	total := 0
	for i := 0; i < count; i++ {
		size, err := readBits(payload[cursor:], bitPos, c.SizeLength)
		if err != nil {
			return 0, fmt.Errorf("%w: AU %d size", ErrUnderivable, i)
		}
		if size == 0 {
			return 0, fmt.Errorf("%w: AU %d empty", ErrUnderivable, i)
		}
		sizes[i] = size
		bitPos += stride
		total += size
		// AUs are concatenated after the header section; the AU-size fields
		// describe the following AU data.
		if total > len(payload)-2-headerBytes {
			return 0, fmt.Errorf("%w: AU sizes exceed payload", ErrUnderivable)
		}
	}
	if total != len(payload)-2-headerBytes {
		return 0, fmt.Errorf("%w: AU sizes cover %d of %d bytes", ErrUnderivable, total, len(payload)-2-headerBytes)
	}
	samples := int64(count) * int64(c.SamplesPerAU)
	return samples * clock.MicrosPerSecond / int64(c.SampleRate), nil
}

func readBits(buf []byte, pos, width int) (int, error) {
	if pos+width > len(buf)*8 {
		return 0, ErrUnderivable
	}
	v := 0
	for i := 0; i < width; i++ {
		byteIdx := (pos + i) / 8
		bitIdx := 7 - (pos+i)%8
		v = v<<1 | int(buf[byteIdx]>>uint(bitIdx)&1)
	}
	return v, nil
}
