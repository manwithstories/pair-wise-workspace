package audio

import (
	"math"

	"pairflow/internal/media"
)

// NormSpec is the pipeline's canonical audio layout: 48 kHz, six channels in
// the order L R C LFE Ls Rs.
var NormSpec = Format{Rate: int(media.AudioRate), Channels: media.AudioChannels}

// Source is a synthesized source clip as delivered by a device. No file is
// referenced; the signal is generated from these fields so a rerun is exact.
type Source struct {
	Rate     int
	Channels int
	Label    string
}

// NewSource validates the source spec. Unknown rates and layouts are errors.
func NewSource(rate, channels int, label, path string) (Source, *media.Error) {
	switch rate {
	case 44100, 48000, 96000:
	default:
		return Source{}, media.Input("audio.unknown_rate",
			"unsupported sample rate %d Hz (only 44100, 48000 and 96000 are known)", rate).WithPath(path)
	}
	switch channels {
	case 1, 2, 6:
	default:
		return Source{}, media.Input("audio.unknown_layout",
			"unsupported channel count %d (only mono, stereo and 5.1 are known)", channels).WithPath(path)
	}
	return Source{Rate: rate, Channels: channels, Label: label}, nil
}

// At returns the continuous signal value of source channel ch at source
// position pos (in source frames). Linear interpolation between neighbouring
// source samples is the sample-rate conversion kernel.
func (s Source) At(ch int, pos float64) float64 {
	if pos <= 0 {
		return s.sample(ch, 0)
	}
	k := math.Floor(pos)
	frac := pos - k
	a := s.sample(ch, int64(k))
	b := s.sample(ch, int64(k)+1)
	return a + (b-a)*frac
}

// sample is the analytic tone of one channel at source frame k. Frequencies
// and phases derive from the label and channel, so heterogeneous clips sound
// different yet reproduce exactly.
func (s Source) sample(ch int, k int64) float64 {
	if ch < 0 || ch >= s.Channels {
		return 0
	}
	base := 220.0 + float64(fnv32(s.Label)%7)*55.0
	f := base + float64(ch)*110.0
	amp := 0.18
	phase := float64(fnv32(s.Label+"#c")%360) * math.Pi / 180.0
	if ch == 3 { // LFE: lower and quieter
		f *= 0.25
		amp = 0.09
	}
	return amp * math.Sin(2*math.Pi*f*float64(k)/float64(s.Rate)+phase+float64(ch)*0.15)
}

// fnv32 is a small deterministic hash for label-derived tone parameters.
func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// Matrix maps source channels into the six canonical channels. Each row lists
// (source index, gain); gain already includes the pan law.
type Matrix struct {
	rows [6][]tap
}

type tap struct {
	src  int
	gain float64
}

// Rows reports how many canonical channels the source fills.
func (m Matrix) Rows() int {
	n := 0
	for _, r := range m.rows {
		if len(r) > 0 {
			n++
		}
	}
	return n
}

// MixFrame computes one canonical frame from a source frame.
func (m Matrix) MixFrame(dst, src []float64) {
	for d := 0; d < 6; d++ {
		var acc float64
		for _, t := range m.rows[d] {
			acc += src[t.src] * t.gain
		}
		dst[d] = acc
	}
}

// minus3dB is the pan gain for mirroring mono into the stereo pair.
const minus3dB = 0.70710678

// PlanMatrix builds the channel map for a known source layout. Unknown counts
// are rejected rather than silently dropped or up-mixed.
//
//	mono   -> L,R at -3 dB, C at unity
//	stereo -> L,R unity
//	5.1    -> identity (L R C LFE Ls Rs)
func PlanMatrix(srcCh int, path string) (Matrix, *media.Error) {
	var m Matrix
	switch srcCh {
	case 6:
		for i := 0; i < 6; i++ {
			m.rows[i] = []tap{{i, 1}}
		}
	case 2:
		m.rows[0] = []tap{{0, 1}}
		m.rows[1] = []tap{{1, 1}}
	case 1:
		m.rows[0] = []tap{{0, minus3dB}}
		m.rows[1] = []tap{{0, minus3dB}}
		m.rows[2] = []tap{{0, 1}}
	default:
		return Matrix{}, media.Input("audio.unknown_layout",
			"unsupported channel count %d (only mono, stereo and 5.1 are known)", srcCh).WithPath(path)
	}
	return m, nil
}

// Span is a clip's coverage in canonical output frames, [Start,End).
type Span struct {
	Start int64
	End   int64
}

// Mix adds one normalized clip into dst, a six-channel interleaved buffer for
// output frames [outStart, outStart+n). Frames outside the clip's span are
// untouched. gain is the track's mix level.
func Mix(dst []int16, outStart int64, span Span, src Source, m Matrix, gain float64) {
	n := len(dst) / 6
	srcFrame := make([]float64, src.Channels)
	mixed := make([]float64, 6)
	for i := 0; i < n; i++ {
		j := outStart + int64(i)
		if j < span.Start || j >= span.End {
			continue
		}
		pos := float64(j) / float64(media.AudioRate) * float64(src.Rate)
		for ch := 0; ch < src.Channels; ch++ {
			srcFrame[ch] = src.At(ch, pos)
		}
		m.MixFrame(mixed, srcFrame)
		for ch := 0; ch < 6; ch++ {
			v := float64(dst[i*6+ch]) + mixed[ch]*gain*32767
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			dst[i*6+ch] = int16(math.Round(v))
		}
	}
}

// OutFrames maps inFrames source frames to canonical frames without rounding
// drift: the result is floor(inFrames*dstRate/srcRate).
func OutFrames(inFrames int, srcRate int) int {
	return int(int64(inFrames) * media.AudioRate / int64(srcRate))
}
