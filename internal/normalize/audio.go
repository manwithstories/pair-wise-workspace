// Package normalize converts parsed AVPK tracks into a canonical render
// representation: PCM audio becomes 48 kHz stereo float32, PNG video frames
// become fixed-size sRGB images. Every conversion keeps exact, integer-based
// bookkeeping so repeated runs produce byte-identical outputs.
package normalize

import (
	"encoding/binary"
	"fmt"

	"avflow/internal/model"
	"avflow/internal/stream"
)

// TargetSampleRate is the canonical audio sample rate.
const TargetSampleRate uint32 = 48000

// PCM samples are signed 16-bit little-endian, interleaved per channel.
const pcmSampleBytes = 2

// AudioFrame is one normalized packet of stereo audio on the nanosecond grid.
type AudioFrame struct {
	Track       uint32
	Seq         uint64
	PTSNS       int64
	EndPTSNS    int64
	Samples     []float32 // interleaved L/R, 2*N values
	InputRate   uint32
	InChannels  uint8
}

// AudioResult aggregates normalized audio plus boundary diagnostics.
type AudioResult struct {
	Frames       []AudioFrame
	TotalSamples int
	Faults       []*model.Fault
}

// NormalizeAudio validates and converts every clean PCM packet of one track.
// Channel-layout and sample-rate conflicts inside the track are reported as
// faults; conflicting packets are skipped rather than silently remixed.
func NormalizeAudio(desc stream.TrackDesc, packets []*stream.Packet) (*AudioResult, error) {
	res := &AudioResult{}
	if desc.Kind != stream.KindAudio {
		return nil, fmt.Errorf("track %d is not an audio track", desc.ID)
	}
	if !validLayout(desc.ChannelLayout, desc.Channels) {
		return res, model.NewFault(model.FaultChannelConflict, desc.ID, 0, 0, 0, false,
			fmt.Sprintf("channels=%d layout=%d inconsistent", desc.Channels, desc.ChannelLayout))
	}
	if desc.SampleRate == 0 {
		return res, model.NewFault(model.FaultRateConflict, desc.ID, 0, 0, 0, false, "zero sample rate")
	}
	pos := model.NewResamplePosition(desc.SampleRate, TargetSampleRate)
	for _, p := range packets {
		f, frame := normalizeOne(desc, p, &pos)
		if f != nil {
			res.Faults = append(res.Faults, f)
			if !f.Recoverable {
				return res, f
			}
		}
		// Recoverable faults (e.g. boundary drift) still carry a converted
		// frame; ragged payloads yield no samples and are skipped.
		if len(frame.Samples) > 0 {
			res.Frames = append(res.Frames, frame)
			res.TotalSamples += len(frame.Samples) / 2
		}
	}
	return res, nil
}

func normalizeOne(desc stream.TrackDesc, p *stream.Packet, pos *model.ResamplePosition) (*model.Fault, AudioFrame) {
	frame := AudioFrame{Track: desc.ID, Seq: p.Seq, PTSNS: p.PTSNS, EndPTSNS: p.EndPTSNS,
		InputRate: desc.SampleRate, InChannels: desc.Channels}
	// Per-packet metadata conflict detection (fixture can mutate descriptors
	// per packet through channel-count carried in payload framing).
	if len(p.Payload)%(int(desc.Channels)*pcmSampleBytes) != 0 {
		return model.NewFault(model.FaultChannelConflict, desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, true,
			"payload size is not a whole number of interleaved frames"), frame
	}
	inSamples := decodePCM(p.Payload, desc.Channels)
	if desc.SampleRate != TargetSampleRate {
		frame.Samples = resampleAndDownmix(inSamples, desc.ChannelLayout, pos)
	} else {
		frame.Samples = downmix(inSamples, desc.ChannelLayout)
	}
	if !model.SampleAligned(p.PTSNS, TargetSampleRate) {
		off := model.SampleOffsetNS(p.PTSNS, TargetSampleRate)
		return model.NewFault(model.FaultBoundaryDrift, desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, true,
			fmt.Sprintf("audio clip point drifts %dns off the 48k grid", off)), frame
	}
	return nil, frame
}

// decodePCM turns interleaved little-endian int16 bytes into per-channel
// sample slices: out[channel][sample].
func decodePCM(payload []byte, channels uint8) [][]float64 {
	frames := len(payload) / (int(channels) * pcmSampleBytes)
	out := make([][]float64, channels)
	for c := range out {
		out[c] = make([]float64, frames)
	}
	for i := 0; i < frames; i++ {
		for c := uint8(0); c < channels; c++ {
			off := (i*int(channels) + int(c)) * pcmSampleBytes
			v := int16(binary.LittleEndian.Uint16(payload[off:]))
			out[c][i] = float64(v) / 32768.0
		}
	}
	return out
}

// downmix maps mono/stereo/5.1/7.1 speaker layouts onto L/R using the
// ITU-R constant-power style weights; mono is duplicated to both channels.
func downmix(in [][]float64, layout uint8) []float32 {
	n := 0
	for _, ch := range in {
		if len(ch) > n {
			n = len(ch)
		}
	}
	out := make([]float32, n*2)
	for i := 0; i < n; i++ {
		var l, r float64
		get := func(c int) float64 {
			if c < len(in) && i < len(in[c]) {
				return in[c][i]
			}
			return 0
		}
		switch layout {
		case stream.LayoutMono:
			l = get(0)
			r = l
		case stream.LayoutStereo:
			l, r = get(0), get(1)
		case stream.Layout51: // L R C LFE BL BR
			l = get(0) + center(get(2)) + surround(get(4))
			r = get(1) + center(get(2)) + surround(get(5))
			l += lfe(get(3))
			r += lfe(get(3))
		case stream.Layout71: // L R C LFE BL BR SL SR
			l = get(0) + center(get(2)) + surround(get(4)) + surround(get(6))
			r = get(1) + center(get(2)) + surround(get(5)) + surround(get(7))
			l += lfe(get(3))
			r += lfe(get(3))
		default:
			l, r = get(0), get(1)
		}
		out[i*2] = clampF32(l)
		out[i*2+1] = clampF32(r)
	}
	return out
}

// resampleAndDownmix walks the integer accumulator: for each output sample it
// picks the corresponding input sample index, then downmixes. Fractional
// positions never round-trip through float, so long runs cannot drift.
func resampleAndDownmix(in [][]float64, layout uint8, pos *model.ResamplePosition) []float32 {
	inFrames := 0
	for _, ch := range in {
		if len(ch) > inFrames {
			inFrames = len(ch)
		}
	}
	// Integer conversion ratio: how many output slots cover inFrames input
	// samples. Rounding up keeps the final partial input frame audible.
	outCount := int((uint64(inFrames)*pos.OutRate() + pos.InRate() - 1) / pos.InRate())
	// Build per-output input frames by stepping the shared cursor.
	picked := make([][]float64, len(in))
	for c := range picked {
		picked[c] = make([]float64, 0, outCount)
	}
	for i := 0; i < outCount; i++ {
		idx := pos.Advance()
		if idx >= uint64(inFrames) {
			idx = uint64(inFrames - 1)
		}
		for c := range in {
			picked[c] = append(picked[c], in[c][idx])
		}
	}
	return downmix(picked, layout)
}

func center(v float64) float64  { return v * 0.70710678 }
func surround(v float64) float64 { return v * 0.70710678 }
func lfe(v float64) float64      { return v * 0.5 }

func clampF32(v float64) float32 {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	return float32(v)
}

func validLayout(layout, channels uint8) bool {
	switch layout {
	case stream.LayoutMono:
		return channels == 1
	case stream.LayoutStereo:
		return channels == 2
	case stream.Layout51:
		return channels == 6
	case stream.Layout71:
		return channels == 8
	default:
		return false
	}
}
