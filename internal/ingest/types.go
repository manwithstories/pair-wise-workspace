package ingest

import (
	"mediarun/internal/clock"
	"mediarun/internal/model"
)

const (
	// MaxPayload bounds a single RTP payload; capture units are small.
	MaxPayload = 1 << 20
	// MaxBatch bounds how many packets a single receive step consumes.
	MaxBatch = 256
	// MinBatch keeps receive steps useful on a very thin link.
	MinBatch = 4
)

// Config configures a Stream.
type Config struct {
	TrackID    uint32
	Timescale  uint32
	Window     int
	SpillDir   string
	SpillLimit int
	// MaxBacklog caps confirmed events held in memory.
	MaxBacklog int
	// StartTick is the unified-clock tick assigned to the first delivered
	// packet of this track. Later packets are placed at StartTick plus
	// their elapsed track time, which is how independent track clocks are
	// aligned on one timeline.
	StartTick clock.Tick
}

// Receipt reports the outcome of one wire packet.
type Receipt struct {
	TrackID   uint32
	Sequence  uint16
	Duplicate bool
	TooOld    bool
	Gaps      []model.Gap
	Spilled   int
}
