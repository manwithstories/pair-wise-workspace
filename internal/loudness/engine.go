package loudness

import (
	"fmt"
	"io"

	"loudcheck/internal/dsp"
	"loudcheck/internal/wavio"
)

type EngineResult struct {
	GateResult
	SampleRate           int     `json:"sample_rate"`
	Channels             int     `json:"channels"`
	TruePeakDBTP         float64 `json:"true_peak_dbtp"`
	SamplePeakLinear     float64 `json:"sample_peak_linear"`
	OversampledPeak      float64 `json:"oversampled_peak_linear"`
	InterSampleOvershoot bool    `json:"intersample_overshoot"`
}

type Engine struct {
	weighters []*dsp.KWeighter
	blocks    *BlockAccumulator
	peak      *dsp.TruePeakMeter
	all       []Block
	header    wavio.Header
	samplePk  float64
	weights   []float64
	used      []bool
}

func NewEngine(header wavio.Header) (*Engine, error) {
	sampleRate := int(header.SampleRate)
	weights, used, err := dsp.ChannelWeights(header.Channels, header.ChannelMask)
	if err != nil {
		return nil, err
	}
	weighters := make([]*dsp.KWeighter, header.Channels)
	for ch := range weighters {
		weighter, err := dsp.NewKWeighter(float64(sampleRate))
		if err != nil {
			return nil, err
		}
		weighters[ch] = weighter
	}
	return &Engine{
		weighters: weighters,
		blocks:    NewBlockAccumulator(sampleRate, weights, used),
		peak:      dsp.NewTruePeakMeter(int(header.Channels)),
		all:       make([]Block, 0, 256),
		header:    header,
		samplePk:  0,
		weights:   weights,
		used:      used,
	}, nil
}

func (e *Engine) Process(planar [][]float64) {
	weighted := make([][]float64, len(planar))
	for ch, samples := range planar {
		for _, value := range samples {
			abs := value
			if abs < 0 {
				abs = -abs
			}
			if abs > e.samplePk {
				e.samplePk = abs
			}
		}
		weighted[ch] = e.weighters[ch].ProcessBuffer(samples)
	}
	e.peak.Process(planar)
	e.all = append(e.all, e.blocks.Process(weighted)...)
}

func (e *Engine) Result() EngineResult {
	gate := Compute(e.all)
	oversampled := e.peak.TruePeakLinear()
	dbtp := e.peak.TruePeakDBTP()
	return EngineResult{
		GateResult:           gate,
		SampleRate:           int(e.header.SampleRate),
		Channels:             int(e.header.Channels),
		TruePeakDBTP:         dbtp,
		SamplePeakLinear:     e.samplePk,
		OversampledPeak:      oversampled,
		InterSampleOvershoot: oversampled > e.samplePk*(1+1e-9),
	}
}

func MeasureStream(r io.Reader, maxFramesPerRead int) (EngineResult, *wavio.Reader, error) {
	reader, err := wavio.NewReader(r)
	if err != nil {
		return EngineResult{}, nil, err
	}
	engine, err := NewEngine(reader.Header())
	if err != nil {
		return EngineResult{}, reader, err
	}
	for {
		planar, readErr := reader.ReadFrames(maxFramesPerRead)
		if len(planar) > 0 {
			engine.Process(planar)
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return EngineResult{}, reader, readErr
		}
	}
	result := engine.Result()
	if result.BlocksTotal == 0 {
		return result, reader, fmt.Errorf("audio shorter than one 400 ms block")
	}
	return result, reader, nil
}
