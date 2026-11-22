package timeline

import (
	"sort"

	"mediascan/internal/media"
	"mediascan/internal/segment"
)

// Kind identifies an event's origin.
type Kind string

const (
	KindSubtitle Kind = "subtitle"
	KindMedia    Kind = "media"
)

// Event is a normalized timeline event.
type Event struct {
	// Seq is the arrival sequence. Ties on EventTime are broken by Seq so the
	// merge is total and deterministic.
	Seq int
	// Kind is the event source subsystem.
	Kind Kind
	// TrackID identifies the source track or subtitle stream.
	TrackID uint32
	// StartNs/EndNs are event-time bounds.
	StartNs int64
	EndNs   int64
	// IssueClasses lists the media issue classes attached to this event; they
	// propagate into the segment's hash so a fault is auditable.
	IssueClasses []string
	// Recovery is the event's own recovery state.
	Recovery segment.Recovery
	// Source locates the event's bytes/packet/cue for reporting.
	Source segment.Source
	// Late marks an event that arrived after the watermark passed its start.
	Late bool
}

// SortEvents orders by (StartNs, Kind, TrackID, Seq).
func SortEvents(evs []Event) []Event {
	out := append([]Event(nil), evs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.StartNs != b.StartNs {
			return a.StartNs < b.StartNs
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		return a.Seq < b.Seq
	})
	return out
}

// Config controls segment boundaries and replay depth.
type Config struct {
	// SegmentNs is the nominal segment length on the timeline.
	SegmentNs int64
	// TrackIDs are the tracks expected to contribute; used to render the
	// segment's track list deterministically.
	TrackIDs []uint32
	// CheckpointEvery commits a checkpoint every N segments; replay resumes
	// from the nearest checkpoint at or before the late event.
	CheckpointEvery int
}

func (c Config) normalize() Config {
	if c.SegmentNs <= 0 {
		c.SegmentNs = media.NanosPerSecond
	}
	if c.CheckpointEvery <= 0 {
		c.CheckpointEvery = 5
	}
	return c
}

// Segment is a committed output unit.
type Segment struct {
	Index    int
	StartNs  int64
	EndNs    int64
	Recovery segment.Recovery
	TrackIDs []uint32
	Sources  []segment.Source
	Classes  []string
	Late     int
	// Checkpoint marks a segment boundary that later replays can resume from.
	Checkpoint bool
	// Hash is the SHA-256 hex digest of the segment's canonical record.
	Hash string
	// Events are the events assigned to this segment (not hashed directly;
	// their normalized facts are).
	Events []Event
}

// HashRecord returns the canonical hashed record for this segment.
func (s Segment) HashRecord() segment.Record {
	return segment.Record{
		Index: s.Index, StartNs: s.StartNs, EndNs: s.EndNs, Recovery: s.Recovery,
		TrackIDs: s.TrackIDs, Sources: s.Sources, IssueClasses: s.Classes, LateEvents: s.Late,
	}
}

// Snapshot is the deterministic check-in state at a segment boundary. Replay
// from a checkpoint re-derives the suffix purely from these facts plus the
// events, so identical inputs always reproduce identical segments.
type Snapshot struct {
	// AfterIndex is the last committed segment index at this checkpoint.
	AfterIndex int
	// WatermarkNs is the watermark at the checkpoint.
	WatermarkNs int64
	// TrackIDs is the set of tracks seen up to this point, sorted.
	TrackIDs []uint32
	// Recovery is the aggregate recovery state.
	Recovery segment.Recovery
}
