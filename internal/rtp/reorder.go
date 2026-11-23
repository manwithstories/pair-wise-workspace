package rtp

import (
	"mediasim/internal/clock"
)

// ReorderOptions are the deterministic transport impairments.
type ReorderOptions struct {
	DuplicateEvery int // duplicate every Nth packet (0 disables)
	SwapEvery      int // swap each Nth packet with its successor (0 disables)
	StartSeq       uint16
}

// Reorderer takes a clean sequence of packets and applies duplication and
// adjacent reordering while preserving the original sequence numbering.
type Reorderer struct {
	opt ReorderOptions
}

// NewReorderer validates the option ranges.
func NewReorderer(opt ReorderOptions) (*Reorderer, error) {
	if opt.DuplicateEvery < 0 || opt.SwapEvery < 0 {
		return nil, clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone,
			"negative impairment period")
	}
	if opt.DuplicateEvery == 1 || opt.SwapEvery == 1 {
		return nil, clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone,
			"impairment period 1 would duplicate or swap every packet")
	}
	return &Reorderer{opt: opt}, nil
}

// Apply returns the wire order. Duplicates are inserted right after their
// source; swaps exchange neighbours so the receiver sees a lower sequence
// number after a higher one.
func (r *Reorderer) Apply(in []Packet) []Packet {
	out := make([]Packet, 0, len(in)+len(in)/2)
	for i, p := range in {
		out = append(out, p)
		if r.opt.DuplicateEvery > 0 && (i+1)%r.opt.DuplicateEvery == 0 {
			out = append(out, p)
		}
	}
	if r.opt.SwapEvery > 0 {
		for i := 0; i+1 < len(out); i++ {
			if (i+1)%r.opt.SwapEvery == 0 {
				out[i], out[i+1] = out[i+1], out[i]
				i++ // do not re-swap the pair
			}
		}
	}
	return out
}
