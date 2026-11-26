package audio

import (
	"mediamix/internal/clock"
)

// PLC performs deterministic packet loss concealment on interleaved stereo
// float32 frames. Lost spans are filled from the last good frame using an
// exponentially decaying hold; every concealed frame is counted so the
// metrics layer can compute an honest recovery rate.
type PLC struct {
	channels       int
	lastGood       []float32
	decay          float64
	concealed      int64
	tailTruncated  int64
	spanLen        int // consecutive concealed frames in the current gap
	maxSpan        int // concealment beyond this span is not attempted
}

// PLCConfig tunes concealment.
type PLCConfig struct {
	Channels   int
	Decay      float64 // per-frame amplitude multiplier, e.g. 0.9995
	MaxSpan    int     // frames; longer gaps are only partially concealed
}

// DefaultPLCConfig returns conservative stereo settings.
func DefaultPLCConfig() PLCConfig {
	return PLCConfig{Channels: 2, Decay: 0.9995, MaxSpan: 48000} // up to 1 s
}

// NewPLC builds a concealer.
func NewPLC(cfg PLCConfig) *PLC {
	if cfg.Channels <= 0 {
		cfg.Channels = 2
	}
	if cfg.Decay <= 0 || cfg.Decay >= 1 {
		cfg.Decay = 0.9995
	}
	if cfg.MaxSpan <= 0 {
		cfg.MaxSpan = 48000
	}
	return &PLC{channels: cfg.Channels, decay: cfg.Decay, maxSpan: cfg.MaxSpan}
}

// Observe records a good frame so subsequent gaps can be concealed.
func (p *PLC) Observe(frame []float32) {
	if len(frame) == 0 {
		return
	}
	cp := make([]float32, len(frame))
	copy(cp, frame)
	p.lastGood = cp
	p.spanLen = 0
}

// Conceal fills exactly missingFrames stereo frames for a detected gap.
// It returns the generated frames and true when concealment succeeded.
// It fails explicitly (false, locatable error) when there is no good frame
// to extrapolate from or the gap is a truncated stream tail, so the caller
// can never mistake a fabricated tail for a successfully recovered packet.
func (p *PLC) Conceal(missingFrames int, truncatedTail bool) ([]float32, bool, error) {
	if missingFrames <= 0 {
		return nil, true, nil
	}
	if truncatedTail {
		p.tailTruncated++
		p.spanLen = 0
		return nil, false, clock.NewError(clock.StagePLC, clock.ErrTruncatedTail, 0, false,
			"stream ends inside a lost span; tail cannot be concealed")
	}
	if len(p.lastGood) < p.channels {
		p.spanLen = 0
		return nil, false, clock.NewError(clock.StagePLC, clock.ErrGap, 0, true,
			"no reference frame available for concealment")
	}
	frames := missingFrames
	if p.spanLen+frames > p.maxSpan {
		frames = p.maxSpan - p.spanLen
	}
	if frames <= 0 {
		return nil, false, clock.NewError(clock.StagePLC, clock.ErrGap, 0, true,
			"concealment span limit reached")
	}
	out := make([]float32, frames*p.channels)
	gain := p.decay
	// Continue decay from the position reached inside the current gap.
	for k := 0; k < p.spanLen; k++ {
		gain *= p.decay
	}
	for f := 0; f < frames; f++ {
		for ch := 0; ch < p.channels; ch++ {
			out[f*p.channels+ch] = float32(float64(p.lastGood[ch]) * gain)
		}
		gain *= p.decay
	}
	p.spanLen += frames
	p.concealed += int64(frames)
	return out, true, nil
}

// ConcealedFrames returns the number of frames successfully synthesised.
func (p *PLC) ConcealedFrames() int64 { return p.concealed }

// TruncatedTails counts gaps refused because they ran to the stream end.
func (p *PLC) TruncatedTails() int64 { return p.tailTruncated }

// Reset clears the reference frame between independent tracks/runs.
func (p *PLC) Reset() {
	p.lastGood = nil
	p.spanLen = 0
}
