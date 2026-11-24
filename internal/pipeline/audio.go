package pipeline

import (
	"fmt"

	"github.com/clearliu/mediq/internal/dsp"
	"github.com/clearliu/mediq/internal/source"
)

// BuildSourceBuffer reconstructs interleaved source-rate audio, filling
// unresolved or rejected sequence slots with deterministic silence.
func BuildSourceBuffer(a *Analysis, spec source.Spec) (*dsp.Buffer, error) {
	totalFrames := spec.PacketCount * spec.FramesPerPacket
	out := &dsp.Buffer{
		Rate:        spec.SampleRate,
		Channels:    spec.Channels,
		Interleaved: make([]float64, 0, totalFrames*spec.Channels),
	}
	silence := make([]float64, spec.FramesPerPacket*spec.Channels)

	for i := 0; i < spec.PacketCount; i++ {
		ext := a.ExtFirst + int64(i)
		pkt, ok := a.Packets[ext]
		if !ok {
			out.Interleaved = append(out.Interleaved, silence...)
			continue
		}
		block, err := dsp.DecodeL16(pkt.Payload, spec.Channels)
		if err != nil {
			return nil, fmt.Errorf("decode seq %d: %w", ext, err)
		}
		if block.Frames() != spec.FramesPerPacket {
			return nil, fmt.Errorf("seq %d has %d frames, want %d", ext, block.Frames(), spec.FramesPerPacket)
		}
		block.Rate = spec.SampleRate
		out.Interleaved = append(out.Interleaved, block.Interleaved...)
	}
	return out, nil
}

// ConvertAudio performs stereo normalization, 48 kHz conversion and R128.
func ConvertAudio(src *dsp.Buffer) ([]byte, dsp.NormalizationResult, *dsp.Buffer, error) {
	stereo := dsp.NormalizeStereo(src)
	rate48 := dsp.Resample(stereo, 48000)
	normalized, norm, err := dsp.NormalizeToR128(rate48, dsp.R128TargetLUFS, dsp.MaxTruePeakDBTP)
	if err != nil {
		return nil, norm, nil, err
	}
	pcm := dsp.EncodeS16LE(normalized)
	measured := dsp.DecodeS16LE(pcm, 48000, 2)
	norm.OutputLUFS = dsp.IntegratedLUFS(measured)
	norm.TruePeakDBTP = dsp.TruePeakDBTP(measured)
	return pcm, norm, measured, nil
}
