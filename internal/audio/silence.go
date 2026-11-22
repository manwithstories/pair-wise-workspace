// Package audio detects silence intervals with a configurable window
// and threshold. The detector is streaming: memory stays bounded by
// one window regardless of track length, and a trailing run shorter
// than one window is never reported.
package audio

import (
	"mediacore/internal/clock"
)

// Params configures silence detection.
type Params struct {
	WindowSamples int   // analysis window length in samples
	Threshold     int16 // peak |sample| at or below this is silence
}

// Segment is one detected silence interval on the shared clock.
type Segment struct {
	Start clock.Tick
	End   clock.Tick
}

// Detector consumes samples incrementally and emits silence segments.
type Detector struct {
	p       Params
	rate    int64
	buf     []int16 // at most one window
	win     int64   // index of the next window
	runFrom int64   // first window of the open silent run, -1 if none
	segs    []Segment
}

// NewDetector validates params and returns a streaming detector.
func NewDetector(sampleRate int, p Params) (*Detector, error) {
	if sampleRate <= 0 {
		return nil, clock.At(clock.CodeTimescaleInvalid, "silence", "", -1,
			"sample rate %d", sampleRate)
	}
	if p.WindowSamples <= 0 {
		return nil, clock.At(clock.CodeManifestInvalid, "silence", "", -1,
			"window %d samples", p.WindowSamples)
	}
	return &Detector{p: p, rate: int64(sampleRate), runFrom: -1}, nil
}

// Feed processes samples; incomplete windows are carried over.
func (d *Detector) Feed(samples []int16) error {
	d.buf = append(d.buf, samples...)
	for len(d.buf) >= d.p.WindowSamples {
		win := d.buf[:d.p.WindowSamples]
		d.buf = append([]int16(nil), d.buf[d.p.WindowSamples:]...)
		d.process(win)
	}
	return nil
}

func (d *Detector) process(win []int16) {
	silent := true
	for _, s := range win {
		if s > d.p.Threshold || s < -d.p.Threshold {
			silent = false
			break
		}
	}
	if silent {
		if d.runFrom < 0 {
			d.runFrom = d.win
		}
	} else if d.runFrom >= 0 {
		d.closeRun(d.win)
	}
	d.win++
}

func (d *Detector) closeRun(endWin int64) error {
	start, err := clock.New(d.runFrom*int64(d.p.WindowSamples), d.rate)
	if err != nil {
		return err
	}
	end, err := clock.New(endWin*int64(d.p.WindowSamples), d.rate)
	if err != nil {
		return err
	}
	d.segs = append(d.segs, Segment{Start: start, End: end})
	d.runFrom = -1
	return nil
}

// Finish closes any open silent run and returns all segments. The
// buffered tail (shorter than one window) is discarded unreported.
func (d *Detector) Finish() ([]Segment, error) {
	if d.runFrom >= 0 {
		if err := d.closeRun(d.win); err != nil {
			return nil, clock.Wrap(clock.CodeTickOverflow, "silence", err)
		}
	}
	d.buf = nil
	return d.segs, nil
}
