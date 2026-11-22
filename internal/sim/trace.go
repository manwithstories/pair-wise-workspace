// Package sim generates fully deterministic, seed-replayable network traces.
// A trace is a sequence of actions over an ideal packet stream: clean
// delivery, duplication, reordering, isolated loss and burst loss, each with
// a simulated arrival time. The same (scenario, seed) pair always yields the
// identical action list, which is what makes frame hashes and metrics
// reproducible across runs.
package sim

import (
	"mediakernel/internal/clock"
)

// ActionKind enumerates what the simulated network does to a packet.
type ActionKind uint8

const (
	ActDeliver ActionKind = iota
	ActDuplicate
	ActReorder
	ActDrop
)

// String renders the action for trace dumps.
func (k ActionKind) String() string {
	switch k {
	case ActDeliver:
		return "deliver"
	case ActDuplicate:
		return "duplicate"
	case ActReorder:
		return "reorder"
	case ActDrop:
		return "drop"
	default:
		return "unknown"
	}
}

// Action is one trace step on one audio stream.
type Action struct {
	Kind   ActionKind
	Stream uint32
	Seq    uint16
	// MediaTime is the packet's ideal timestamp on the unified clock.
	MediaTime clock.Ticks
	// Arrival is the simulated receive time, including jitter and reordering.
	Arrival clock.Ticks
	// OriginalIndex preserves the ideal ordering for hash stability.
	OriginalIndex int
}

// BandwidthSample is one point on the simulated available-bandwidth curve.
type BandwidthSample struct {
	At          clock.Ticks
	BytesPerSec float64
}

// Trace is a complete replayable impairment schedule.
type Trace struct {
	Scenario  string
	Seed      int64
	Actions   []Action
	Bandwidth []BandwidthSample
	// LostSeqs lists audio sequence numbers removed by the trace.
	LostSeqs []uint16
	// BurstRanges lists inclusive [from,to] loss bursts.
	BurstRanges [][2]uint16
}

// Params controls trace construction.
type Params struct {
	// Packets is the ideal audio packet count.
	Packets int
	// FramesPerPacket is the audio frame count per RTP packet.
	FramesPerPacket int
	// AudioSSRC identifies the audio stream.
	AudioSSRC uint32
	// BaseInterval is the ideal inter-packet media spacing.
	BaseInterval clock.Ticks
}

// DefaultParams returns 48kHz/20ms-packet simulation parameters. 300 packets
// span six seconds, matching the default 180-frame/30fps video fixture so the
// audio and video timelines cover the same interval.
func DefaultParams() Params {
	return Params{
		Packets:         300,
		FramesPerPacket: 960,
		AudioSSRC:       0xC0FFEE,
		BaseInterval:    clock.Milliseconds(20),
	}
}
