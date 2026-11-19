// Package audio parses integer PCM/WAVE blocks, maps declared channel
// layouts, resamples deterministically and measures loudness.
package audio

import (
	"encoding/binary"

	"avpipe/internal/fault"
)

// Info is the parsed WAV container header.
type Info struct {
	Format      Format `json:"format"`
	DataOffset  int64  `json:"data_offset"`
	DataBytes   int64  `json:"data_bytes"`
	TotalBytes  int64  `json:"total_bytes"`
	BlockFrames int    `json:"-"`
}

// Format mirrors the WAV fmt chunk plus the resolved layout. The layout is
// not part of the on-disk format; it is derived from the channel mask or the
// channel count and kept as a declared contract for downstream stages.
type Format struct {
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
	Layout        Layout `json:"layout"`
	ChannelMask   uint32 `json:"channel_mask,omitempty"`
}

// BlockAlign is bytes per interleaved frame.
func (f Format) BlockAlign() int { return f.Channels * f.BitsPerSample / 8 }

const (
	wavFormatPCM        = 1
	wavFormatExtensible = 0xFFFE
	minSampleRate       = 4000
	maxSampleRate       = 384000
	maxChannels         = 8
)

// Parse validates a complete RIFF/WAVE image held in memory and returns the
// container header. Every rejection names the byte offset of the offending
// field. expectedLayout, when non-empty, is checked for channel-count
// agreement with the file.
func Parse(src []byte, expectedLayout string) (Info, error) {
	if len(src) < 12 {
		return Info{}, fault.New(fault.CodeWAVTruncated, "audio.wav.parse",
			"file is %d bytes, need at least 12", len(src)).At(int64(len(src)))
	}
	if string(src[0:4]) != "RIFF" || string(src[8:12]) != "WAVE" {
		return Info{}, fault.New(fault.CodeWAVFormat, "audio.wav.parse",
			"missing RIFF/WAVE magic").At(0)
	}
	riffSize := int64(binary.LittleEndian.Uint32(src[4:8])) + 8
	if riffSize > int64(len(src)) {
		return Info{}, fault.New(fault.CodeWAVLength, "audio.wav.parse",
			"RIFF declares %d bytes but file has %d", riffSize, len(src)).At(4)
	}
	end := int64(len(src))
	if riffSize >= 12 && riffSize < end {
		end = riffSize
	}

	var (
		fmtSeen  bool
		dataSeen bool
		info     Info
	)
	for off := int64(12); off+8 <= end; {
		id := string(src[off : off+4])
		size := int64(binary.LittleEndian.Uint32(src[off+4 : off+8]))
		payload := off + 8
		if size < 0 || payload+size > end {
			return Info{}, fault.New(fault.CodeWAVLength, "audio.wav.parse",
				"chunk %q declares %d bytes at offset %d but only %d remain",
				id, size, payload, end-payload).At(payload)
		}
		switch id {
		case "fmt ":
			if fmtSeen {
				return Info{}, fault.New(fault.CodeWAVFormat, "audio.wav.parse",
					"duplicate fmt chunk").At(off)
			}
			f, err := parseFmt(src[payload:payload+size], payload, expectedLayout)
			if err != nil {
				return Info{}, err
			}
			info.Format, fmtSeen = f, true
		case "data":
			if !fmtSeen {
				return Info{}, fault.New(fault.CodeWAVFormat, "audio.wav.parse",
					"data chunk precedes fmt chunk").At(off)
			}
			if dataSeen {
				return Info{}, fault.New(fault.CodeWAVFormat, "audio.wav.parse",
					"duplicate data chunk").At(off)
			}
			info.DataOffset, info.DataBytes, dataSeen = payload, size, true
		}
		adv := payload + size
		if size%2 == 1 {
			adv++
		}
		off = adv
	}
	if !fmtSeen {
		return Info{}, fault.New(fault.CodeWAVFormat, "audio.wav.parse", "missing fmt chunk").At(12)
	}
	if !dataSeen {
		return Info{}, fault.New(fault.CodeWAVFormat, "audio.wav.parse", "missing data chunk").At(end)
	}
	align := info.Format.BlockAlign()
	if align <= 0 || info.DataBytes%int64(align) != 0 {
		return Info{}, fault.New(fault.CodeWAVLength, "audio.wav.parse",
			"data length %d is not a multiple of block align %d",
			info.DataBytes, align).At(info.DataOffset)
	}
	info.TotalBytes = int64(len(src))
	info.BlockFrames = int(info.DataBytes) / align
	return info, nil
}

func parseFmt(b []byte, off int64, expectedLayout string) (Format, error) {
	if len(b) < 16 {
		return Format{}, fault.New(fault.CodeWAVTruncated, "audio.wav.fmt",
			"fmt chunk is %d bytes, need 16", len(b)).At(off)
	}
	tag := binary.LittleEndian.Uint16(b[0:2])
	channels := int(binary.LittleEndian.Uint16(b[2:4]))
	rate := int(binary.LittleEndian.Uint32(b[4:8]))
	byteRate := binary.LittleEndian.Uint32(b[8:12])
	align := int(binary.LittleEndian.Uint16(b[12:14]))
	bits := int(binary.LittleEndian.Uint16(b[14:16]))

	if channels <= 0 || channels > maxChannels {
		return Format{}, fault.New(fault.CodeWAVFormat, "audio.wav.fmt",
			"unsupported channel count %d (1..%d)", channels, maxChannels).At(off + 2)
	}
	if rate < minSampleRate || rate > maxSampleRate {
		return Format{}, fault.New(fault.CodeWAVFormat, "audio.wav.fmt",
			"sample rate %d outside %d..%d", rate, minSampleRate, maxSampleRate).At(off + 4)
	}
	switch bits {
	case 8, 16, 24, 32:
	default:
		return Format{}, fault.New(fault.CodeWAVFormat, "audio.wav.fmt",
			"unsupported PCM bit depth %d (8/16/24/32)", bits).At(off + 14)
	}
	if align != channels*bits/8 {
		return Format{}, fault.New(fault.CodeWAVFormat, "audio.wav.fmt",
			"block align %d disagrees with %dch x %dbit (%d)",
			align, channels, bits, channels*bits/8).At(off + 12)
	}
	if byteRate != uint32(rate*align) {
		return Format{}, fault.New(fault.CodeWAVFormat, "audio.wav.fmt",
			"byte rate %d disagrees with %d x %d", byteRate, rate, align).At(off + 8)
	}
	var mask uint32
	switch tag {
	case wavFormatPCM:
	case wavFormatExtensible:
		if len(b) < 40 {
			return Format{}, fault.New(fault.CodeWAVTruncated, "audio.wav.fmt",
				"extensible fmt chunk is %d bytes, need 40", len(b)).At(off)
		}
		if binary.LittleEndian.Uint16(b[24:26]) != wavFormatPCM {
			return Format{}, fault.New(fault.CodeWAVFormat, "audio.wav.fmt",
				"extensible subformat is not integer PCM").At(off + 24)
		}
		mask = binary.LittleEndian.Uint32(b[20:24])
	default:
		return Format{}, fault.New(fault.CodeWAVFormat, "audio.wav.fmt",
			"unsupported format tag 0x%04x (integer PCM only)", tag).At(off)
	}
	layout, err := resolveLayout(channels, mask)
	if err != nil {
		e := &fault.Error{Code: fault.CodeUnsupportedLayout, Op: "audio.wav.fmt",
			Detail: err.Error(), Cause: err}
		return Format{}, e.At(off)
	}
	if expectedLayout != "" {
		want, err := ParseLayout(expectedLayout)
		if err != nil {
			return Format{}, err
		}
		if len(want.Channels) != channels {
			return Format{}, fault.New(fault.CodeUnsupportedLayout, "audio.wav.fmt",
				"declared layout %s has %d channels but file has %d",
				want.Name, len(want.Channels), channels).At(off)
		}
	}
	return Format{SampleRate: rate, Channels: channels, BitsPerSample: bits,
		Layout: layout, ChannelMask: mask}, nil
}

func resolveLayout(channels int, mask uint32) (Layout, error) {
	if mask != 0 {
		if l, ok := MaskToLayout(mask, channels); ok {
			return l, nil
		}
		return Layout{}, fault.New(fault.CodeUnsupportedLayout, "audio.wav.fmt",
			"channel mask 0x%x with %d channels has no supported layout", mask, channels)
	}
	return LayoutForChannels(channels)
}
