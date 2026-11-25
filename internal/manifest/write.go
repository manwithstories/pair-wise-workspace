// Package manifest writes the segment playlist. Each entry records the
// deterministic segment id, timeline window and publishability, so an edge
// node can skip CC-gap/clock-gap segments and a restarted job never republishes
// an id it has already shipped.
package manifest

import (
	"encoding/json"
	"io"

	"streamcut/internal/media"
)

// Entry is one playlist row. Times are extended 90 kHz media ticks.
type Entry struct {
	ID          string        `json:"id"`
	Index       int           `json:"index"`
	StartTicks  media.Time90  `json:"start_ticks"`
	EndTicks    media.Time90  `json:"end_ticks"`
	StartMS     float64       `json:"start_ms"`
	EndMS       float64       `json:"end_ms"`
	Publishable bool          `json:"publishable"`
	DuplicateOf string        `json:"duplicate_of,omitempty"`
}

// Manifest is the full playlist plus the timing summary used to compute
// manifest_skew_ms against the source timeline.
type Manifest struct {
	TargetDurationMS int     `json:"target_duration_ms"`
	Entries          []Entry `json:"entries"`
	// FirstStart / LastEnd bound the covered media timeline.
	FirstStart media.Time90 `json:"-"`
	LastEnd    media.Time90  `json:"-"`
}

// Add appends a segment entry, keeping FirstStart/LastEnd current.
func (m *Manifest) Add(e Entry) {
	if len(m.Entries) == 0 || e.StartTicks < m.FirstStart {
		m.FirstStart = e.StartTicks
	}
	if e.EndTicks > m.LastEnd {
		m.LastEnd = e.EndTicks
	}
	m.Entries = append(m.Entries, e)
}

// PublishedIDs returns the ids that were actually publishable and not
// duplicates, in playlist order — the restart checkpoint set.
func (m *Manifest) PublishedIDs() []string {
	out := make([]string, 0, len(m.Entries))
	for _, e := range m.Entries {
		if e.Publishable && e.DuplicateOf == "" {
			out = append(out, e.ID)
		}
	}
	return out
}

// WriteJSON serializes the manifest with stable key order (struct order).
func WriteJSON(w io.Writer, m *Manifest) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// SkewMS measures manifest timing accuracy as the largest absolute deviation
// of any segment boundary (start or end) from the ideal target-duration grid.
// The grid is anchored at the playlist's own first start rather than tick zero,
// so a stream whose extended timeline begins at an arbitrary epoch (notably a
// 33-bit PCR/PTS wrap) is judged on boundary regularity, not its absolute base.
func (m *Manifest) SkewMS(sourceDuration media.Time90) float64 {
	if len(m.Entries) == 0 {
		return 0
	}
	grid := media.Time90(m.TargetDurationMS) * media.TicksPerSecond / 1000
	origin := m.FirstStart
	var worst media.Time90
	for _, e := range m.Entries {
		for _, tick := range [2]media.Time90{e.StartTicks, e.EndTicks} {
			rel := tick - origin
			rem := rel % grid
			dev := rem
			if grid-rem < dev {
				dev = grid - rem
			}
			// The stream's own end is a legal boundary even if it falls
			// short of a full final grid cell.
			if tick == e.EndTicks && rel >= sourceDuration-grid {
				continue
			}
			if dev > worst {
				worst = dev
			}
		}
	}
	return worst.Milliseconds()
}
