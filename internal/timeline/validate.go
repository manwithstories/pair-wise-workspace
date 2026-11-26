package timeline

import (
	"fmt"

	"github.com/relayteam/relay/internal/media"
)

// StreamKey identifies one ordered representation of a track.
type StreamKey struct {
	Track     string `json:"track"`
	Rendition string `json:"rendition"`
}

func (r Record) key() StreamKey {
	return StreamKey{Track: r.Track, Rendition: r.Rendition}
}

// Validator enforces per-track ordering and clip/transition relationships while
// streaming. Its memory is O(number of track/rendition streams), not records.
type Validator struct {
	last map[StreamKey]Record
}

// NewValidator creates an empty streaming validator.
func NewValidator() *Validator {
	return &Validator{last: make(map[StreamKey]Record)}
}

// Validate checks one record and retains the latest valid record per stream.
// The returned error preserves the first bad record's track, number and PTS.
func (v *Validator) Validate(rec *Record) error {
	if rec == nil {
		return media.E(media.ErrMalformedRecord, "timeline.validate", media.Meta{RecordNo: -1}, nil)
	}
	meta := rec.Meta(rec.RecordNo, rec.LineNo)
	if rec.Dur90k <= 0 {
		return media.E(media.ErrMalformedRecord, "timeline.validate", meta,
			fmt.Errorf("dur90k must be positive, got %d", rec.Dur90k))
	}
	if rec.Transition != nil && rec.Transition.Endpoint == "" {
		return media.E(media.ErrTransitionEndpoint, "timeline.validate", meta,
			fmt.Errorf("transition %q has no endpoint", rec.Transition.ID))
	}

	key := rec.key()
	if prev, ok := v.last[key]; ok {
		prevMeta := prev.Meta(prev.RecordNo, prev.LineNo)
		if rec.PTS90k < prev.PTS90k {
			err := media.E(media.ErrTimestampOrder, "timeline.validate", meta, fmt.Errorf(
				"pts90k=%d precedes previous record=%d pts90k=%d",
				rec.PTS90k, prevMeta.RecordNo, prev.PTS90k))
			return err
		}
		if rec.Seq < prev.Seq {
			return media.E(media.ErrSequenceOrder, "timeline.validate", meta, fmt.Errorf(
				"seq=%d precedes previous record=%d seq=%d", rec.Seq, prev.RecordNo, prev.Seq))
		}
		// A clip transition must name either the outgoing or incoming clip.
		if tr := rec.Transition; tr != nil {
			if tr.FromClip != "" && tr.FromClip != prev.Clip && tr.FromClip != rec.Clip {
				return media.E(media.ErrTransitionEndpoint, "timeline.validate", meta, fmt.Errorf(
					"from_clip=%q is neither previous clip %q nor current clip %q",
					tr.FromClip, prev.Clip, rec.Clip))
			}
		}
	}
	cp := *rec
	v.last[key] = cp
	return nil
}

// ValidateAll validates an in-memory slice and returns the first bad record.
func ValidateAll(records []Record) error {
	v := NewValidator()
	for i := range records {
		if records[i].RecordNo == 0 {
			records[i].RecordNo = i + 1
			records[i].LineNo = i + 1
		}
		if err := v.Validate(&records[i]); err != nil {
			return err
		}
	}
	return nil
}

// FirstBadRecord extracts the one-based source record from an error. Zero
// means no source location is available.
func FirstBadRecord(err error) int {
	me, ok := media.First(err)
	if !ok {
		return 0
	}
	return me.Meta.RecordNo
}
