package fixture

import "archive/internal/clock"

// Descriptor carries the absolute presentation timeline of one
// synthetic segment, independent of how the counters are stored. A
// wrapped segment reports a raw counter that wrapped mid-stream while
// its descriptor start stays continuous, so consumers can tell a
// genuine wrap from a real timeline jump.
type Descriptor struct {
	Index     int
	Media     string // manifest-relative segment path
	Start     clock.Rational
	Dur       clock.Rational
	VideoRaw  uint64 // first stored video counter (may be post-wrap)
	AudioRaw  uint64
	Wrapped   bool // a counter wrapped inside this segment
	VideoTS   uint32
	AudioTS   uint32
	VideoBase int64
	AudioBase int64
}

// Descriptors returns the presentation timeline of the layout that
// Generate wrote with the same options.
func Descriptors(opt Options) ([]Descriptor, error) {
	opt = opt.normalize()
	if opt.Segments > 100000 {
		return nil, nil
	}
	out := make([]Descriptor, 0, opt.Segments)
	for i := 0; i < opt.Segments; i++ {
		vbase, abase := segmentBases(i, opt)
		d := Descriptor{
			Index:     i,
			Media:     "media/" + segName(i),
			Start:     clock.Must(int64(i*SegDurSecs), 1),
			Dur:       clock.Must(SegDurSecs, 1),
			VideoRaw:  uint64(vbase),
			AudioRaw:  uint64(abase),
			VideoTS:   VideoTimesc,
			AudioTS:   AudioTimesc,
			VideoBase: vbase,
			AudioBase: abase,
		}
		vEnd := uint64(vbase) + uint64(videoFrames*3600)
		aEnd := uint64(abase) + uint64(audioFrames*960)
		d.Wrapped = vEnd > 1<<33 || aEnd > 1<<32
		out = append(out, d)
	}
	return out, nil
}

// segmentBases mirrors segment(): presentation-true first counters,
// offset by the wrap displacement when Wrap is set.
func segmentBases(idx int, opt Options) (video, audio int64) {
	vbase := int64(idx) * SegDurSecs * VideoTimesc
	abase := int64(idx)*SegDurSecs*AudioTimesc + AudioLeadTicks
	if opt.Wrap {
		const wrapAt = 40 * SegDurSecs
		vbase += (1 << 33) - int64(wrapAt)*VideoTimesc
		abase += (1 << 32) - int64(wrapAt)*AudioTimesc
	}
	return vbase, abase
}

func segName(i int) string {
	s := "seg-"
	b := make([]byte, 5)
	for p := 4; p >= 0; p-- {
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return s + string(b) + ".m4s"
}
