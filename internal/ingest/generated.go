package ingest

import (
	"mediacheck/internal/clock"
	"mediacheck/internal/model"
)

// Generated is one deterministic fixture stream ready to feed the pipeline.
type Generated struct {
	Config     Config
	Audio      *Stream
	Video      *Stream
	AudioTotal int
	VideoTotal int
	VideoSlots []int64      // per frame start in nanoseconds
	AudioSlots []model.Slot // per packet span in nanoseconds
	Assets     *mediaAssets
	Span       int64 // media span in nanoseconds
}

// Stream is an ordered packet source for one track.  The generator fills it
// completely and Reader replays it through the pipeline.
type Stream struct {
	Track    model.Track
	Timebase clock.Timebase
	Packets  []model.Packet
	Sequence uint32
}
