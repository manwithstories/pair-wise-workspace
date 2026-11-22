package demo

import (
	"fmt"

	"github.com/clearliu/fieldcut/internal/ingest"
	"github.com/clearliu/fieldcut/internal/timeline"
	"github.com/pion/rtp"
)

// handleOrdered feeds reorder-buffer output into the watermark and then
// appends every released event to the replay engine. Gap packets become
// untrusted gap events: fieldcut never fabricates a frame for a hole.
func handleOrdered(wm *timeline.Watermark, engine *timeline.ReplayEngine, ordered []ingest.OrderedPacket) {
	for _, op := range ordered {
		if op.IsGap {
			gap := &timeline.Event{
				TrackID:  0,
				Sequence: int(op.GapStart),
				Kind:     timeline.KindGap,
				Start:    0,
				Defect:   fmt.Sprintf("gap:%d", op.GapCount),
			}
			released, _ := wm.Commit(gap)
			for _, r := range released {
				engine.Append(r)
			}
			continue
		}
		ev := orderedEvent(op.Packet, 0, op.Recovered, op.Source)
		released, _ := wm.Commit(ev)
		for _, r := range released {
			engine.Append(r)
		}
	}
}

// orderedEvent converts a validated RTP packet into a timeline event with
// its recovery attribution attached.
func orderedEvent(pkt *rtp.Packet, track int, recovered bool, src ingest.Source) *timeline.Event {
	return &timeline.Event{
		TrackID:   track,
		Sequence:  int(pkt.SequenceNumber),
		Kind:      timeline.KindFrame,
		RawTS:     pkt.Timestamp,
		Source:    src,
		Recovered: recovered,
	}
}
