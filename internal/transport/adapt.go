package transport

import (
	"time"

	"relaycheck/internal/media"
)

// Transition records a bandwidth step boundary.
type Transition struct {
	Segment  int `json:"segment"`
	FromKbps int `json:"from_kbps"`
	ToKbps   int `json:"to_kbps"`
}

// Pacer maps ordered bandwidth steps onto the segment timeline and derives
// send cadence. It is deterministic and uses integer duration math only.
type Pacer struct {
	steps        []int
	segmentCount int
	safetyFactor int // percent of bandwidth usable for media, 1..100
}

// NewPacer creates a pacer over segmentCount segments. At least one positive
// bandwidth step is required.
func NewPacer(kbpsSteps []int, segmentCount int) (*Pacer, *media.Fault) {
	if segmentCount <= 0 {
		return nil, media.NewFault(media.CodeInvalidInput, "segmentCount must be positive, got %d", segmentCount)
	}
	if len(kbpsSteps) == 0 {
		return nil, media.NewFault(media.CodeInvalidInput, "at least one bandwidth step is required")
	}
	for _, k := range kbpsSteps {
		if k <= 0 {
			return nil, media.NewFault(media.CodeInvalidInput, "bandwidth must be positive kbps, got %d", k)
		}
	}
	return &Pacer{steps: append([]int{}, kbpsSteps...), segmentCount: segmentCount, safetyFactor: 85}, nil
}

// SetSafetyFactor sets the usable-media percentage (1..100).
func (p *Pacer) SetSafetyFactor(percent int) *media.Fault {
	if percent <= 0 || percent > 100 {
		return media.NewFault(media.CodeInvalidInput, "safety factor must be 1..100, got %d", percent)
	}
	p.safetyFactor = percent
	return nil
}

// StepIndex returns which bandwidth step governs segment i.
func (p *Pacer) StepIndex(segment int) int {
	if segment < 0 {
		segment = 0
	}
	// Equal-width bands; the final step absorbs the remainder.
	idx := segment * len(p.steps) / p.segmentCount
	if idx >= len(p.steps) {
		idx = len(p.steps) - 1
	}
	return idx
}

// RateKbps returns the raw bandwidth for segment i.
func (p *Pacer) RateKbps(segment int) int { return p.steps[p.StepIndex(segment)] }

// MediaBitrateKbps returns the safe target media bitrate for segment i.
func (p *Pacer) MediaBitrateKbps(segment int) int {
	return p.RateKbps(segment) * p.safetyFactor / 100
}

// Pace returns the serialization airtime for payloadBytes at segment i's rate.
// duration_ns = bytes*8*1e9 / (kbps*1000) = bytes*8*1e6/kbps.
func (p *Pacer) Pace(segment, payloadBytes int) time.Duration {
	kbps := p.RateKbps(segment)
	if payloadBytes <= 0 {
		return 0
	}
	ns := int64(payloadBytes) * 8 * 1_000_000 / int64(kbps)
	return time.Duration(ns)
}

// Transitions lists the segment boundaries where the rate changes.
func (p *Pacer) Transitions() []Transition {
	var out []Transition
	prev := p.steps[0]
	for seg := 0; seg < p.segmentCount; seg++ {
		rate := p.RateKbps(seg)
		if rate != prev {
			out = append(out, Transition{Segment: seg, FromKbps: prev, ToKbps: rate})
			prev = rate
		}
	}
	return out
}
