package audio

import (
	"github.com/asticode/go-astiav"

	"mediamix/internal/clock"
)

// readInterleavedF32 copies exactly frames stereo frames out of the frame's
// interleaved float32 plane, ignoring any linesize alignment padding.
func readInterleavedF32(f *astiav.Frame, frames int) ([]float32, error) {
	if f.SampleFormat() != astiav.SampleFormatFlt {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidParam, 0, false,
			"destination frame must be interleaved float")
	}
	channels := f.ChannelLayout().Channels()
	if frames <= 0 || channels <= 0 {
		return nil, nil
	}
	n := frames * channels
	if f.Linesize()[0] < n*4 {
		return nil, clock.NewError(clock.StageConvert, clock.ErrInvalidPacket, 0, false,
			"destination frame plane smaller than declared samples")
	}
	return readF32(f, n), nil
}
