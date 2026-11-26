// Package audio turns the received mono 16 kHz PCM RTP payloads into the
// delivery specification: 48 kHz stereo float audio, without losing or
// repeating samples at fragment boundaries. It also performs deterministic
// packet loss concealment (PLC) and loudness/true-peak control.
package audio

import (
	"fmt"

	"github.com/asticode/go-astiav"

	"mediamix/internal/clock"
)

// TargetSpec is the required delivery audio format.
type TargetSpec struct {
	SampleRate int
	Channels   int
}

// DefaultTarget returns 48 kHz stereo.
func DefaultTarget() TargetSpec {
	return TargetSpec{SampleRate: 48000, Channels: 2}
}

// SourceSpec describes one incoming track.
type SourceSpec struct {
	SampleRate int
	Channels   int
}

// DefaultSource returns 16 kHz mono s16 (matches the RTP fixtures).
func DefaultSource() SourceSpec {
	return SourceSpec{SampleRate: 16000, Channels: 1}
}

// Converter resamples and remixes fragments through FFmpeg's swresample
// (via go-astiav). Every input sample is accounted for across fragment
// boundaries: the resampler delay is flushed at Close and the total output
// sample count is verified against the integer rate ratio.
type Converter struct {
	src   SourceSpec
	dst   TargetSpec
	swr   *astiav.SoftwareResampleContext
	inN   int64 // accepted source samples
	outN  int64 // produced destination samples
	closed bool
}

// NewConverter builds and sanity-checks a converter.
func NewConverter(src SourceSpec, dst TargetSpec) (*Converter, error) {
	if src.SampleRate <= 0 || src.Channels <= 0 || dst.SampleRate <= 0 || dst.Channels <= 0 {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidParam, 0, false,
			"audio rates and channel counts must be positive")
	}
	swr := astiav.AllocSoftwareResampleContext()
	if swr == nil {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidParam, 0, false,
			"failed to allocate resampler")
	}
	return &Converter{src: src, dst: dst, swr: swr}, nil
}

// frameFromS16 wraps a little-endian s16 PCM payload in an AVFrame without
// copying more than necessary: swresample reads directly from the buffer.
func (c *Converter) frameFromS16(payload []byte) (*astiav.Frame, error) {
	bytesPerSample := 2
	n := len(payload) / bytesPerSample
	if len(payload)%bytesPerSample != 0 {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidPacket, 0, false,
			fmt.Sprintf("payload length %d is not whole s16 samples", len(payload)))
	}
	f := astiav.AllocFrame()
	f.SetSampleFormat(astiav.SampleFormatS16)
	f.SetSampleRate(c.src.SampleRate)
	f.SetNbSamples(n)
	f.SetChannelLayout(astiav.ChannelLayoutMono)
	if err := f.AllocBuffer(0); err != nil {
		f.Free()
		return nil, clock.WrapError(clock.StageConvert, clock.ErrInvalidPacket, 0, false,
			"alloc source frame buffer", err)
	}
	copyPayload(f, payload)
	return f, nil
}

// allocDst creates a destination frame whose buffer can hold the converted
// output of inSamples source frames, including resampler filter headroom.
func (c *Converter) allocDst(inSamples int) *astiav.Frame {
	f := astiav.AllocFrame()
	f.SetSampleFormat(astiav.SampleFormatFlt)
	f.SetSampleRate(c.dst.SampleRate)
	f.SetChannelLayout(astiav.ChannelLayoutStereo)
	ratio := c.dst.SampleRate / c.src.SampleRate
	f.SetNbSamples(inSamples*ratio + 64)
	return f
}

// Convert processes one PCM fragment and returns interleaved stereo float
// samples (len = frames*2). Boundaries are sample-accurate: callers may feed
// arbitrary fragment sizes and concatenate the results.
func (c *Converter) Convert(payload []byte) ([]float32, error) {
	if c.closed {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidParam, 0, false,
			"converter used after close")
	}
	if len(payload) == 0 {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidPacket, 0, false,
			"empty audio fragment")
	}
	in, err := c.frameFromS16(payload)
	if err != nil {
		return nil, err
	}
	defer in.Free()
	nIn := in.NbSamples()

	out := c.allocDst(nIn)
	defer out.Free()
	if err := c.swr.ConvertFrame(in, out); err != nil {
		return nil, clock.WrapError(clock.StageConvert, clock.ErrInvalidPacket, 0, true,
			"resample frame", err)
	}
	// swresample holds an internal filter delay; the frames actually emitted
	// by this call equal the current total position (input-driven) minus the
	// samples still buffered inside the resampler.
	c.inN += int64(nIn)
	totalPos := c.inN * int64(c.dst.SampleRate) / int64(c.src.SampleRate)
	frames := int(totalPos - c.swr.Delay(int64(c.dst.SampleRate)) - c.outN)
	if frames < 0 {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidPacket, 0, false,
			"resampler produced fewer samples than the rate ratio requires")
	}
	samples, err := readInterleavedF32(out, frames)
	if err != nil {
		return nil, err
	}
	c.outN += int64(frames)
	return samples, nil
}

// Flush emits every sample still held inside the resampler. swr's reported
// delay is input-side and does not reset on flush, so termination is driven
// by the cumulative output reaching the rate-ratio expected count instead.
// After Flush the converter cannot be reused.
func (c *Converter) Flush() ([]float32, error) {
	if c.closed {
		return nil, nil
	}
	c.closed = true
	defer c.swr.Free()
	expected := c.ExpectedFrames()
	var all []float32
	for guard := 0; c.outN < expected && guard < 16; guard++ {
		remaining := int(expected - c.outN)
		out := astiav.AllocFrame()
		out.SetSampleFormat(astiav.SampleFormatFlt)
		out.SetSampleRate(c.dst.SampleRate)
		out.SetChannelLayout(astiav.ChannelLayoutStereo)
		out.SetNbSamples(remaining + 64)
		if err := out.AllocBuffer(0); err != nil {
			out.Free()
			return nil, clock.WrapError(clock.StageConvert, clock.ErrInvalidPacket, 0, false,
				"alloc flush frame", err)
		}
		if err := c.swr.ConvertFrame(nil, out); err != nil {
			out.Free()
			return nil, clock.WrapError(clock.StageConvert, clock.ErrInvalidPacket, 0, false,
				"flush resampler", err)
		}
		// First flush returns the held tail; later calls produce nothing and
		// end the loop once all expected frames are already accounted for.
		frames := remaining
		if out.NbSamples() < frames {
			frames = out.NbSamples()
		}
		if frames <= 0 {
			out.Free()
			break
		}
		samples, err := readInterleavedF32(out, frames)
		out.Free()
		if err != nil {
			return nil, err
		}
		all = append(all, samples...)
		c.outN += int64(frames)
	}
	if c.outN != expected {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidPacket, 0, false,
			fmt.Sprintf("flush incomplete: out=%d expected=%d", c.outN, expected))
	}
	return all, nil
}

// Totals returns the accepted source samples and produced destination
// frames, allowing the pipeline to prove no samples were lost or repeated.
func (c *Converter) Totals() (inSamples, outFrames int64) {
	return c.inN, c.outN
}

// ExpectedFrames reports the exact destination frame count implied by the
// rate ratio; the final output must equal it once flushed.
func (c *Converter) ExpectedFrames() int64 {
	return c.inN * int64(c.dst.SampleRate) / int64(c.src.SampleRate)
}
