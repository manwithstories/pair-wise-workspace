// Package meter orchestrates the measurement pipeline: WAV decode →
// channel weighting → K-weighting → windowed energy → gated integration,
// with a parallel oversampled true-peak branch. It streams in fixed-size
// blocks, so memory stays constant regardless of file duration.
package meter

import (
	"io"
	"math"
	"time"

	"loudcheck/internal/audio"
	"loudcheck/internal/filter"
	"loudcheck/internal/gate"
	"loudcheck/internal/peak"
	"loudcheck/internal/wav"
)

// blockFrames is the streaming read granularity.
const blockFrames = 4096

// Options configures a measurement.
type Options struct {
	Layout        audio.Layout  // declared layout; LayoutAuto derives from channels
	ShortWindow   time.Duration // short-term window, default 3s
	PeakFactor    int           // oversampling factor 4 or 8, default 4
	TargetLUFS    float64       // integrated loudness ceiling, e.g. -23
	TruePeakLimit float64       // dBTP ceiling, e.g. -1.0
	Tolerance     float64       // LU slack above target, default 0.5
}

func (o Options) withDefaults() Options {
	if o.ShortWindow <= 0 {
		o.ShortWindow = 3 * time.Second
	}
	if o.PeakFactor == 0 {
		o.PeakFactor = 4
	}
	if o.Tolerance == 0 {
		o.Tolerance = 0.5
	}
	return o
}

// Decision values.
const (
	DecisionPass        = "pass"
	DecisionFail        = "fail"
	DecisionUndecidable = "undecidable"
)

// Result is the measurement outcome for one file.
type Result struct {
	File           string
	Layout         string
	LFEIncluded    bool
	IntegratedLUFS *float64
	ShortLUFSMax   *float64
	TruePeakDBTP   *float64
	Samples        int64
	Windows        int64
	GatedWindows   int64
	Duration       time.Duration
	LoudnessPass   bool
	TruePeakPass   bool
	Decision       string
	Reason         string
}

// MeasureFile measures one WAV file. A non-nil error means the file
// could not be processed at all (bad format, I/O error, NaN samples).
func MeasureFile(path string, opts Options) (*Result, error) {
	opts = opts.withDefaults()
	start := time.Now()

	r, err := wav.Open(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f := r.Format()

	layout, err := audio.ResolveLayout(opts.Layout, f.Channels)
	if err != nil {
		return nil, err
	}
	weights, lfe := audio.Weights(layout, f.Channels)

	kw := make([]*filter.KWeight, f.Channels)
	tp := make([]*peak.TruePeak, f.Channels)
	for c := range kw {
		if kw[c], err = filter.NewKWeight(f.SampleRate); err != nil {
			return nil, err
		}
		if tp[c], err = peak.NewTruePeak(opts.PeakFactor); err != nil {
			return nil, err
		}
	}
	stHops := int(opts.ShortWindow.Milliseconds() + 50)
	stHops = (stHops + gate.HopMs - 1) / gate.HopMs
	acc := gate.NewAccumulator(weights, f.SampleRate, stHops)

	buf := make([]float64, blockFrames*f.Channels)
	var samples int64
	for {
		n, err := r.ReadFrames(buf)
		for i := 0; i < n; i++ {
			frame := buf[i*f.Channels : (i+1)*f.Channels]
			for c, x := range frame {
				tp[c].Process(x)
				frame[c] = kw[c].Process(x)
			}
			acc.AddFrame(frame)
		}
		samples += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	var tpMax float64
	for _, t := range tp {
		t.Finish()
		if t.Max() > tpMax {
			tpMax = t.Max()
		}
	}

	res := &Result{
		File:        path,
		Layout:      layout.String(),
		LFEIncluded: lfe,
		Samples:     samples,
		Windows:     acc.Blocks(),
		Duration:    time.Since(start),
	}

	integrated, gated, ok := acc.Integrated()
	res.GatedWindows = gated
	if ok {
		res.IntegratedLUFS = &integrated
	}
	if st, ok := acc.ShortTermMax(); ok {
		res.ShortLUFSMax = &st
	}
	if db, ok := peak.DBTP(tpMax); ok {
		db = math.Round(db*1000) / 1000
		res.TruePeakDBTP = &db
	}

	res.decide(opts, ok)
	return res, nil
}

func (res *Result) decide(opts Options, integratedOK bool) {
	res.TruePeakPass = true
	if res.TruePeakDBTP != nil && *res.TruePeakDBTP > opts.TruePeakLimit {
		res.TruePeakPass = false
	}
	if !integratedOK {
		res.Decision = DecisionUndecidable
		res.Reason = "integrated loudness undecidable: all windows gated (silence or too short)"
		return
	}
	res.LoudnessPass = *res.IntegratedLUFS <= opts.TargetLUFS+opts.Tolerance
	switch {
	case res.LoudnessPass && res.TruePeakPass:
		res.Decision = DecisionPass
		res.Reason = "within target"
	case !res.LoudnessPass && !res.TruePeakPass:
		res.Decision = DecisionFail
		res.Reason = "integrated loudness exceeds target; true peak exceeds limit"
	case !res.LoudnessPass:
		res.Decision = DecisionFail
		res.Reason = "integrated loudness exceeds target"
	default:
		res.Decision = DecisionFail
		res.Reason = "true peak exceeds limit"
	}
}
