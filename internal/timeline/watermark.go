// Package timeline owns the playout watermark and deterministic replay of
// late-arriving events. The watermark is the highest media time whose output
// has been sealed; events at or below it are stable, events above it may still
// be reordered by the jitter window.
package timeline

import (
	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

// Watermark advances the sealing frontier over the unified media timeline and
// remembers sealed intervals so that late packets can be recognized.
type Watermark struct {
	mark    clock.Ticks
	started bool
	// sealedFrom is the lower bound ever sealed; intervals are contiguous.
	sealedFrom clock.Ticks
	log        *model.Log
	advances   int
}

// NewWatermark creates a watermark at the origin.
func NewWatermark(log *model.Log) *Watermark {
	return &Watermark{log: log}
}

// Advance moves the frontier to t. Only forward movement takes effect; the
// returned span is the newly sealed (previous, t] interval. When t does not
// advance the frontier a zero-length span is returned.
func (wm *Watermark) Advance(t clock.Ticks) Span {
	if !wm.started {
		wm.started = true
		wm.mark = t
		wm.sealedFrom = t
		wm.advances++
		return Span{From: t, To: t, Empty: true}
	}
	if t <= wm.mark {
		return Span{From: wm.mark, To: wm.mark, Empty: true}
	}
	prev := wm.mark
	wm.mark = t
	wm.advances++
	return Span{From: prev, To: t}
}

// IsSealed reports whether an event at the given media time can no longer be
// changed by ordinary window reordering.
func (wm *Watermark) IsSealed(t clock.Ticks) bool {
	if !wm.started {
		return false
	}
	return t <= wm.mark
}

// Mark returns the current frontier.
func (wm *Watermark) Mark() clock.Ticks { return wm.mark }

// SealedFrom returns the lowest time ever sealed.
func (wm *Watermark) SealedFrom() clock.Ticks { return wm.sealedFrom }

// Advances counts forward frontier movements.
func (wm *Watermark) Advances() int { return wm.advances }

// Span describes a sealed half-open-ish interval [From, To].
type Span struct {
	From  clock.Ticks
	To    clock.Ticks
	Empty bool
}
