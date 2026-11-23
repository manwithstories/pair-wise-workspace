// Package manifest builds the archive manifest from segment statistics
// and validates it: no overlapping intervals, no unexpected holes, and
// index references aligned with segment boundaries.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
	"mediarchive/internal/index"
	"mediarchive/internal/repair"
	"mediarchive/internal/safepath"
	"mediarchive/internal/segment"
	"mediarchive/internal/timeline"
)

// boundaryTolerance is how far a segment start may drift from its index
// entry before it counts as misalignment: half a 20 ms packet.
const boundaryTolerance = clock.Time(900) // 10 ms

// Gap is the JSON view of a missing interval.
type Gap struct {
	AfterSeq   uint64  `json:"after_seq"`
	Count      uint64  `json:"count"`
	Start      int64   `json:"start"`
	End        int64   `json:"end"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
}

// Mark is the JSON view of an unrepairable event.
type Mark struct {
	SeqExt    uint64 `json:"seq_ext"`
	MediaTime int64  `json:"media_time"`
	Reason    string `json:"reason"`
}

// SegmentRef is the JSON view of one media segment.
type SegmentRef struct {
	ID       int    `json:"id"`
	Path     string `json:"path"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	StartSeq uint64 `json:"start_seq"`
	Samples  int    `json:"samples"`
	SHA256   string `json:"sha256"`
}

// Manifest is the archive manifest for one recovered stream.
type Manifest struct {
	Codec        string       `json:"codec"`
	Channels     int          `json:"channels"`
	ClockRate    uint32       `json:"clock_rate"`
	Timebase     int          `json:"timebase"`
	Segments     []SegmentRef `json:"segments"`
	Gaps         []Gap        `json:"gaps"`
	Unrepairable []Mark       `json:"unrepairable"`
	Quarantined  int          `json:"quarantined"`
	Degraded     []string     `json:"degraded"`
	TimelineSHA  string       `json:"timeline_sha256"`
}

// Build assembles the manifest from pipeline outputs.
func Build(cfg event.CodecConfig, infos []segment.Info, gaps []repair.Gap,
	marks []timeline.UnrepairableMark, quarantined int, degraded []repair.Degradation,
	timelineSHA string) *Manifest {
	m := &Manifest{
		Codec:       cfg.Codec.String(),
		Channels:    int(cfg.Channels),
		ClockRate:   cfg.ClockRate,
		Timebase:    clock.Rate,
		Quarantined: quarantined,
		TimelineSHA: timelineSHA,
	}
	for i, in := range infos {
		m.Segments = append(m.Segments, SegmentRef{
			ID: i, Path: in.Path, Start: int64(in.Start), End: int64(in.End),
			StartSeq: in.StartSeq, Samples: in.Samples, SHA256: in.SHA256,
		})
	}
	for _, g := range gaps {
		m.Gaps = append(m.Gaps, Gap{
			AfterSeq: g.AfterSeq, Count: g.Count, Start: int64(g.Start),
			End: int64(g.End), Source: g.Source, Confidence: g.Confidence,
		})
	}
	for _, u := range marks {
		m.Unrepairable = append(m.Unrepairable, Mark{
			SeqExt: u.SeqExt, MediaTime: int64(u.MediaTime), Reason: string(u.Reason),
		})
	}
	for _, d := range degraded {
		m.Degraded = append(m.Degraded, fmt.Sprintf("%s: %s", d.Reason, d.Detail))
	}
	return m
}

// Validate checks the manifest against the jump index and returns the
// alignment error in unified ticks plus any structural problems found:
// overlapping segments, holes not explained by declared gaps or
// unrepairable marks, and segment starts without a matching index entry.
// Each unrepairable mark contributes one nominal packet duration to the
// alignment error: it is timeline the manifest cannot position.
func (m *Manifest) Validate(idx *index.Index, nominalDur clock.Time) (clock.Time, []string) {
	var problems []string
	var align clock.Time
	for i, s := range m.Segments {
		if i > 0 {
			prev := m.Segments[i-1]
			if s.Start < prev.End {
				problems = append(problems, fmt.Sprintf("segment %d overlaps segment %d", s.ID, prev.ID))
			}
			if hole := s.Start - prev.End; hole > 0 && !m.holeDeclared(prev.End, s.Start) {
				problems = append(problems, fmt.Sprintf("undeclared hole of %d ticks before segment %d", hole, s.ID))
			}
		}
		e, ok := idx.Find(clock.Time(s.Start))
		if !ok || clock.Time(s.Start)-e.Time > boundaryTolerance || e.Time-clock.Time(s.Start) > boundaryTolerance {
			problems = append(problems, fmt.Sprintf("segment %d start has no aligned index entry", s.ID))
			align = max(align, nominalDur)
		} else if d := absDiff(clock.Time(s.Start), e.Time); d > align {
			align = d
		}
	}
	for range m.Unrepairable {
		align += nominalDur
	}
	return align, problems
}

// holeDeclared reports whether the open interval (from, to) is explained
// by a declared gap or by unrepairable marks inside it.
func (m *Manifest) holeDeclared(from, to int64) bool {
	for _, g := range m.Gaps {
		if int64(g.Start) <= from+int64(boundaryTolerance) && (g.End == 0 || int64(g.End) >= to-int64(boundaryTolerance)) {
			return true
		}
	}
	for _, u := range m.Unrepairable {
		if u.MediaTime > from && u.MediaTime <= to {
			return true
		}
	}
	return false
}

// Write stores the manifest as JSON under dir, rejecting path traversal.
func (m *Manifest) Write(dir string) error {
	path, err := safepath.Join(dir, "manifest.json")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func absDiff(a, b clock.Time) clock.Time {
	if a > b {
		return a - b
	}
	return b - a
}

func max(a, b clock.Time) clock.Time {
	if a > b {
		return a
	}
	return b
}
