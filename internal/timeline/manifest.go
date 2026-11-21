package timeline

import (
	"sort"

	"mkvarchive/internal/model"
)

// TrackRange is one archived track's observed timeline, in absolute
// nanoseconds on the unified clock.
type TrackRange struct {
	SourceTrackUID uint64          `json:"source_track_uid"`
	ArchiveTrackID uint64          `json:"archive_track_id"`
	Kind           model.TrackKind `json:"kind"`
	StartNS        int64           `json:"start_ns"`
	EndNS          int64           `json:"end_ns"`
	Blocks         int             `json:"blocks"`
}

// StartMS / EndMS expose the rounded millisecond views QA checks consume.
func (r TrackRange) StartMS() int64 { return RoundMillis(r.StartNS) }
func (r TrackRange) EndMS() int64   { return RoundMillis(r.EndNS) }

// Manifest is the long-term reviewable record of an archival run. All
// timestamps derive from the same nanosecond clock so identical inputs yield
// identical manifests.
type Manifest struct {
	SchemaVersion  string               `json:"schema_version"`
	TimestampScale uint64               `json:"timestamp_scale_ns"`
	Tracks         []TrackRange         `json:"tracks"`
	Warnings       []model.ArchiveError `json:"json_warnings,omitempty"`
}

// SchemaVersion is bumped whenever the manifest contract changes.
const SchemaVersion = "1.0"

// Builder accumulates per-track timeline observations in streaming order and
// finalizes them into a sorted, deterministic Manifest.
type Builder struct {
	clock    Clock
	ranges   map[uint64]*TrackRange
	warnings []model.ArchiveError
}

// NewBuilder starts a manifest builder for the segment timestamp scale.
func NewBuilder(timestampScale uint64) *Builder {
	return &Builder{
		clock:  NewClock(timestampScale),
		ranges: map[uint64]*TrackRange{},
	}
}

// Clock exposes the unified clock so block timecodes convert consistently.
func (b *Builder) Clock() Clock { return b.clock }

// Observe records one block's absolute raw timecode for a source track.
func (b *Builder) Observe(uid, archiveID uint64, kind model.TrackKind, rawTimecode int64) {
	ns := b.clock.ToNanoseconds(rawTimecode)
	r, ok := b.ranges[uid]
	if !ok {
		r = &TrackRange{
			SourceTrackUID: uid,
			ArchiveTrackID: archiveID,
			Kind:           kind,
			StartNS:        ns,
			EndNS:          ns,
		}
		b.ranges[uid] = r
	}
	if ns < r.StartNS {
		r.StartNS = ns
	}
	if ns > r.EndNS {
		r.EndNS = ns
	}
	r.Blocks++
}

// AddWarning carries a non-fatal conflict (metadata, color, alignment) into
// the manifest instead of silently normalizing it.
func (b *Builder) AddWarning(w *model.ArchiveError) {
	if w != nil {
		b.warnings = append(b.warnings, *w)
	}
}

// Build finalizes the manifest; tracks sort by archive id for stability.
func (b *Builder) Build() *Manifest {
	out := make([]TrackRange, 0, len(b.ranges))
	for _, r := range b.ranges {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ArchiveTrackID != out[j].ArchiveTrackID {
			return out[i].ArchiveTrackID < out[j].ArchiveTrackID
		}
		return out[i].SourceTrackUID < out[j].SourceTrackUID
	})
	return &Manifest{
		SchemaVersion:  SchemaVersion,
		TimestampScale: b.clock.ScaleNS(),
		Tracks:         out,
		Warnings:       b.warnings,
	}
}

// AlignmentErrorMS compares an archived block's position against the
// manifest expectation using the shared nanosecond rounding rule.
func AlignmentErrorFor(actualRawTC, expectedRawTC int64, scale uint64) float64 {
	c := NewClock(scale)
	return AlignmentErrorMS(c.ToNanoseconds(actualRawTC), c.ToNanoseconds(expectedRawTC))
}
