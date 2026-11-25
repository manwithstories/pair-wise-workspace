// Package sync aligns the audio sample clock (48 kHz) and the video media
// clock (90 kHz) onto one wall-time reference and quantifies their drift.
package sync

import (
	"math"
	"sort"
	"time"

	"avlab/internal/media"
)

// AudioClockRate is the reference audio sample rate in hertz.
const AudioClockRate uint32 = 48_000

// Sample is one clock observation from either track.
type Sample struct {
	// Tick is the observation on the unified 90 kHz media clock.
	Tick media.MediaTime
	// AudioSample is the equivalent position on the 48 kHz audio clock.
	AudioSample uint64
	// IsVideo marks observations originating from the video track.
	IsVideo bool
}

// ClockMapping converts between audio samples, video ticks and wall time.
type ClockMapping struct {
	originTick   media.MediaTime
	originAudio  uint64
	audioPerTick float64
}

// NewClockMapping anchors both clocks at one common reference point.
func NewClockMapping(originTick media.MediaTime, originAudio uint64) *ClockMapping {
	return &ClockMapping{
		originTick:   originTick,
		originAudio:  originAudio,
		audioPerTick: float64(AudioClockRate) / float64(media.ClockRate),
	}
}

// AudioToTick projects an audio sample position onto the 90 kHz clock.
func (m *ClockMapping) AudioToTick(sample uint64) (media.MediaTime, error) {
	delta := float64(sample-m.originAudio) / m.audioPerTick
	if delta > float64(media.MaxTick) || delta < float64(media.MinTick()) {
		return 0, media.NewFault(media.CodeTimestampOverflow, false,
			"audio-to-video projection out of range")
	}
	r, err := m.originTick.Add(media.MediaTime(math.Round(delta)))
	return r, err
}

// TickToAudio projects a 90 kHz tick onto the audio sample clock.
func (m *ClockMapping) TickToAudio(tick media.MediaTime) uint64 {
	delta := float64(tick-m.originTick) * m.audioPerTick
	return m.originAudio + uint64(math.Round(delta))
}

// Drift is one signed audio-vs-video offset at a paired observation.
type Drift struct {
	Tick       media.MediaTime
	OffsetMS   float64
	AbsOffsetMS float64
}

// DriftReport summarizes the paired observations.
type DriftReport struct {
	Pairs     int
	MeanMS    float64
	P50MS     float64
	P95MS     float64
	MaxAbsMS  float64
	Threshold float64
	Failed    bool
	Failures  int
}

// Aligner pairs audio and video observations against the common mapping and
// evaluates whether their drift stays inside the configured threshold.
type Aligner struct {
	mapping   *ClockMapping
	threshold time.Duration
	drifts    []Drift
}

// NewAligner creates an aligner with the given failure threshold.
func NewAligner(mapping *ClockMapping, threshold time.Duration) *Aligner {
	return &Aligner{mapping: mapping, threshold: threshold}
}

// Observe records one video tick together with the audio sample observed at
// the same wall instant. A deterministic, bounded offset may be supplied to
// emulate capture jitter; zero means a perfectly aligned pair.
func (a *Aligner) Observe(videoTick media.MediaTime, audioSample uint64) error {
	expectedAudio := a.mapping.TickToAudio(videoTick)
	offsetSamples := int64(audioSample) - int64(expectedAudio)
	offsetMS := float64(offsetSamples) * 1000 / float64(AudioClockRate)
	d := Drift{
		Tick:        videoTick,
		OffsetMS:    offsetMS,
		AbsOffsetMS: math.Abs(offsetMS),
	}
	a.drifts = append(a.drifts, d)
	return nil
}

// Report computes mean/percentile drift and flags threshold violations.
func (a *Aligner) Report() *DriftReport {
	r := &DriftReport{Threshold: float64(a.threshold.Milliseconds())}
	if len(a.drifts) == 0 {
		return r
	}
	abs := make([]float64, len(a.drifts))
	var sum float64
	for i, d := range a.drifts {
		abs[i] = d.AbsOffsetMS
		sum += d.AbsOffsetMS
		if d.AbsOffsetMS > r.Threshold {
			r.Failures++
		}
	}
	sort.Float64s(abs)
	r.Pairs = len(a.drifts)
	r.MeanMS = sum / float64(len(a.drifts))
	r.P50MS = percentile(abs, 50)
	r.P95MS = percentile(abs, 95)
	r.MaxAbsMS = abs[len(abs)-1]
	r.Failed = r.Failures > 0
	return r
}

// Drifts returns the recorded drift series (ordered by insertion).
func (a *Aligner) Drifts() []Drift { return a.drifts }

// percentile returns the nearest-rank percentile of a sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
