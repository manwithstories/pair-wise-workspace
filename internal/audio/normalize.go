// Package audio streams s16le PCM and normalizes any supported channel
// layout and sample rate to 48 kHz stereo. Unsupported layouts or rates
// are unrecoverable: they are never silently rewritten.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrUnsupported marks unrecoverable layout/rate combinations.
var ErrUnsupported = errors.New("audio: unsupported layout or sample rate")

// Layout is a PCM channel layout.
type Layout int

const (
	LayoutUnknown Layout = iota
	LayoutMono
	LayoutStereo
	LayoutFiveOne
)

// ParseLayout parses "mono", "stereo" and "5.1".
func ParseLayout(s string) (Layout, bool) {
	switch s {
	case "mono":
		return LayoutMono, true
	case "stereo":
		return LayoutStereo, true
	case "5.1":
		return LayoutFiveOne, true
	default:
		return LayoutUnknown, false
	}
}

// Channels returns the interleaved channel count, or 0 when unsupported.
func (l Layout) Channels() int {
	switch l {
	case LayoutMono:
		return 1
	case LayoutStereo:
		return 2
	case LayoutFiveOne:
		return 6
	default:
		return 0
	}
}

// Spec describes an input PCM stream.
type Spec struct {
	Layout Layout
	Rate   int
}

func (s Spec) validate() error {
	if s.Layout.Channels() == 0 {
		return fmt.Errorf("%w: layout", ErrUnsupported)
	}
	switch s.Rate {
	case 44100, 48000, 96000:
		return nil
	default:
		return fmt.Errorf("%w: %d Hz", ErrUnsupported, s.Rate)
	}
}

type stereoFrame struct{ L, R int16 }

func clamp16(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// toStereo maps one input frame (ch samples) to stereo. 5.1 is downmixed
// as L' = L + 0.707*C + 0.707*Ls (LFE dropped), mirrored for R.
func toStereo(layout Layout, s []int16) stereoFrame {
	switch layout {
	case LayoutMono:
		return stereoFrame{s[0], s[0]}
	case LayoutStereo:
		return stereoFrame{s[0], s[1]}
	default: // 5.1: L R C LFE Ls Rs
		l := (int32(s[0])*1024 + int32(s[2])*724 + int32(s[4])*724) >> 10
		r := (int32(s[1])*1024 + int32(s[2])*724 + int32(s[5])*724) >> 10
		return stereoFrame{clamp16(l), clamp16(r)}
	}
}

// resampler converts input frames to 48 kHz with linear interpolation.
// The output position is tracked as an exact rational k*num/den to avoid
// floating-point drift in output frame counts.
type resampler struct {
	num, den  int64 // input frames per output frame, reduced
	k         int64 // output frame index
	n         int64 // frames pushed
	prev, cur stereoFrame
}

func newResampler(rate int) *resampler {
	g := gcd(int64(rate), 48000)
	return &resampler{num: int64(rate) / g, den: 48000 / g}
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (rs *resampler) push(f stereoFrame, emit func(stereoFrame)) {
	rs.prev, rs.cur = rs.cur, f
	rs.n++
	for rs.k*rs.num < (rs.n-1)*rs.den {
		emit(lerp(rs.prev, rs.cur, rs.frac()))
		rs.k++
	}
}

func (rs *resampler) flush(emit func(stereoFrame)) {
	for rs.k*rs.num < rs.n*rs.den {
		emit(rs.cur) // clamped tail frame
		rs.k++
	}
}

// frac returns the fractional position of output k between frames n-2, n-1.
func (rs *resampler) frac() float64 {
	return float64((rs.k*rs.num)%rs.den) / float64(rs.den)
}

func lerp(a, b stereoFrame, t float64) stereoFrame {
	return stereoFrame{
		L: int16(math.Round(float64(a.L)*(1-t) + float64(b.L)*t)),
		R: int16(math.Round(float64(a.R)*(1-t) + float64(b.R)*t)),
	}
}

const chunkFrames = 2048

// Normalize streams s16le PCM from r, converts it to 48 kHz stereo s16le
// and writes it to w, returning the number of output sample frames.
func Normalize(r io.Reader, w io.Writer, spec Spec) (int64, error) {
	if err := spec.validate(); err != nil {
		return 0, err
	}
	ch := spec.Layout.Channels()
	rs := newResampler(spec.Rate)
	var written int64
	out := make([]byte, 0, chunkFrames*4)
	emit := func(f stereoFrame) {
		var b [4]byte
		binary.LittleEndian.PutUint16(b[0:], uint16(f.L))
		binary.LittleEndian.PutUint16(b[2:], uint16(f.R))
		out = append(out, b[:]...)
		written++
	}
	flushOut := func() error {
		if len(out) == 0 {
			return nil
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
		out = out[:0]
		return nil
	}
	buf := make([]byte, chunkFrames*ch*2)
	frame := make([]int16, ch)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			n -= n % (ch * 2) // drop incomplete trailing frame
			for i := 0; i < n; i += ch * 2 {
				for c := 0; c < ch; c++ {
					frame[c] = int16(binary.LittleEndian.Uint16(buf[i+c*2:]))
				}
				rs.push(toStereo(spec.Layout, frame), emit)
			}
			if len(out) >= chunkFrames*4 {
				if err := flushOut(); err != nil {
					return written, err
				}
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return written, err
		}
	}
	rs.flush(emit)
	if err := flushOut(); err != nil {
		return written, err
	}
	return written, nil
}
