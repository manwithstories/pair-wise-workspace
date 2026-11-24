package txn

import (
	"encoding/json"
	"fmt"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// SegmentPayload is the canonical on-disk form of a segment. It contains the
// merged events of the slice plus the gaps that touch it, so an independent
// verifier can recompute the hash from the file alone.
type SegmentPayload struct {
	Version  int           `json:"version"`
	Name     string        `json:"name"`
	Start    clock.Tick    `json:"start"`
	End      clock.Tick    `json:"end"`
	Events   []model.Event `json:"events"`
	Gaps     []model.Gap   `json:"gaps,omitempty"`
	TrackIDs []uint32      `json:"track_ids"`
}

// NewSegmentPayload builds the payload for one slice of the timeline.
func NewSegmentPayload(name string, start, end clock.Tick, events []model.Event, gaps []model.Gap) *SegmentPayload {
	tracks := map[uint32]bool{}
	for _, ev := range events {
		tracks[ev.TrackID] = true
	}
	var ids []uint32
	for id := range tracks {
		ids = append(ids, id)
	}
	sortUint32(ids)
	return &SegmentPayload{
		Version:  1,
		Name:     name,
		Start:    start,
		End:      end,
		Events:   events,
		Gaps:     gaps,
		TrackIDs: ids,
	}
}

// Encode serialises the payload deterministically. Events are written in
// merge order; the encoder emits no maps so key ordering cannot vary.
func (p *SegmentPayload) Encode() ([]byte, error) {
	if p.Version != 1 {
		return nil, fmt.Errorf("%w: segment payload version %d", model.ErrInvalid, p.Version)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func sortUint32(v []uint32) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
