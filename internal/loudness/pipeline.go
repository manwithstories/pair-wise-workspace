package loudness

import (
	"errors"
	"io"

	"loudness/internal/kweight"
	"loudness/internal/riff"
)

const defaultChunkFrames = 8192

// Processor owns reusable filter, analyzer and interpolator state. A batch
// worker keeps one Processor and calls ProcessFile repeatedly.
type Processor struct {
	designs  map[int]kweight.Design
	bank     *kweight.Bank
	analyzer *Analyzer
	tp       *TruePeak
	input    []float64
	weighted []float64
}

func NewProcessor() *Processor {
	return &Processor{designs: make(map[int]kweight.Design)}
}

// ProcessFile decodes and measures one WAV file without retaining audio.
func (p *Processor) ProcessFile(path string) (Metrics, error) {
	d, err := riff.Open(path)
	if err != nil {
		return Metrics{}, err
	}
	defer d.Close()

	f := d.Format()
	gains, err := f.ChannelGains()
	if err != nil {
		return Metrics{}, err
	}
	design, ok := p.designs[f.SampleRate]
	if !ok {
		design, err = kweight.ForRate(f.SampleRate)
		if err != nil {
			return Metrics{}, err
		}
		p.designs[f.SampleRate] = design
	}
	if p.bank == nil {
		p.bank, err = kweight.NewBank(design, f.Channels)
	} else {
		err = p.bank.Reset(design, f.Channels)
	}
	if err != nil {
		return Metrics{}, err
	}
	if p.analyzer == nil {
		p.analyzer, err = NewAnalyzer(f.SampleRate, f.Channels, gains)
	} else {
		err = p.analyzer.Reset(f.SampleRate, f.Channels, gains)
	}
	if err != nil {
		return Metrics{}, err
	}
	if p.tp == nil {
		p.tp, err = NewTruePeak(f.Channels)
	} else {
		err = p.tp.Reset(f.Channels)
	}
	if err != nil {
		return Metrics{}, err
	}

	need := defaultChunkFrames * f.Channels
	if cap(p.input) < need {
		p.input = make([]float64, need)
	} else {
		p.input = p.input[:need]
	}
	if cap(p.weighted) < need {
		p.weighted = make([]float64, need)
	} else {
		p.weighted = p.weighted[:need]
	}

	for {
		frames, readErr := d.ReadFrames(p.input, defaultChunkFrames)
		if frames > 0 {
			n := frames * f.Channels
			if err := p.bank.Process(p.weighted[:n], p.input[:n], frames); err != nil {
				return Metrics{}, err
			}
			if err := p.analyzer.Add(p.weighted[:n], frames); err != nil {
				return Metrics{}, err
			}
			if err := p.tp.Add(p.input[:n], frames); err != nil {
				return Metrics{}, err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return Metrics{}, readErr
		}
	}
	m := p.analyzer.Finish()
	m.TruePeakDBTP = p.tp.DBTP()
	m.NaNs = d.NaNs()
	return m, nil
}
