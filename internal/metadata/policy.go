package metadata

import (
	"fmt"
	"sort"

	"mediasync/internal/model"
)

// Source describes where one metadata value originated.
type Source struct {
	Name     string // container/track source label
	Priority int    // explicit policy priority; higher wins
}

// ConflictPolicy is the explicit adjudication rule set. Every conflict records
// the winning source instead of silently picking a value.
type ConflictPolicy struct {
	// RotationPriority ranks rotation sources (e.g. container vs sidecar).
	RotationPriority map[string]int
	// CropPriority ranks clean-aperture sources.
	CropPriority map[string]int
	// ColorPriority ranks color-description sources.
	ColorPriority map[string]int
}

// DefaultPolicy prefers explicit sidecar declarations, then MXF, then MP4.
func DefaultPolicy() ConflictPolicy {
	return ConflictPolicy{
		RotationPriority: map[string]int{"sidecar": 3, "mxf": 2, "mp4": 1},
		CropPriority:     map[string]int{"sidecar": 3, "mxf": 2, "mp4": 1},
		ColorPriority:    map[string]int{"sidecar": 3, "mxf": 2, "mp4": 1},
	}
}

func (p ConflictPolicy) rank(table map[string]int, source string) int {
	if r, ok := table[source]; ok {
		return r
	}
	return 0
}

// Decision records the outcome of one field adjudication.
type Decision struct {
	Field   string // rotation|crop|color
	Winner  string
	Loser   string
	TrackID string
	Message string
}

// ResolvedTrack is a TrackMeta plus the adjudication trail and surviving issues.
type ResolvedTrack struct {
	Track     TrackMeta
	Decisions []Decision
}

// Resolve merges multiple normalized records describing the same logical track
// and applies the explicit policy. Structural errors (bad rotation, out-of-bounds
// crop, color conflicts) are always recorded with their track and timestamp.
func Resolve(p ConflictPolicy, records []TrackMeta) (ResolvedTrack, model.Bag) {
	var diag model.Bag
	if len(records) == 0 {
		return ResolvedTrack{}, diag
	}
	out := ResolvedTrack{Track: records[0]}
	merged := map[string]TrackMeta{}
	for _, rec := range records {
		merged[rec.Source] = rec
	}
	out.Track = pickBase(records)

	rotation := adjudicate(p.RotationPriority, records, func(t TrackMeta) bool { return t.HasRot })
	if rotation != nil {
		rec := *rotation
		if rec.Rotation%90 != 0 {
			diag.Add(model.Diagnostic{Kind: model.KindBadRotation, Severity: model.SeverityWarning,
				Track: rec.TrackID, AtTicks: model.NoTimestamp, Recoverable: true,
				Message: fmt.Sprintf("rotation %d not cardinal; normalized to nearest multiple of 90", rec.Rotation)})
			rec.Rotation = ((rec.Rotation + 45) / 90) * 90 % 360
			if rec.Rotation < 0 {
				rec.Rotation += 360
			}
		}
		out.Track.HasRot, out.Track.Rotation = true, rec.Rotation
		out.Decisions = append(out.Decisions, decisionsFor("rotation", records, rec.Source)...)
	}

	crop := adjudicate(p.CropPriority, records, func(t TrackMeta) bool { return t.Crop != nil })
	if crop != nil && crop.Crop != nil {
		c := crop.Crop
		if c.X < 0 || c.Y < 0 || c.X+c.Width > crop.Width || c.Y+c.Height > crop.Height ||
			crop.Width == 0 || crop.Height == 0 {
			diag.Add(model.Diagnostic{Kind: model.KindCropOutOfBounds, Severity: model.SeverityError,
				Track: crop.TrackID, AtTicks: model.NoTimestamp,
				Message: fmt.Sprintf("crop (%d,%d %dx%d) outside frame %dx%d from %s",
					c.X, c.Y, c.Width, c.Height, crop.Width, crop.Height, crop.Source)})
			// Fall back to the next-priority in-bounds candidate instead of
			// dropping the field entirely.
			for _, cand := range rankedRecords(p.CropPriority, records, func(t TrackMeta) bool { return t.Crop != nil }) {
				if cand.Source == crop.Source {
					continue
				}
				cc := cand.Crop
				if cc.X >= 0 && cc.Y >= 0 && cand.Width > 0 && cand.Height > 0 &&
					cc.X+cc.Width <= cand.Width && cc.Y+cc.Height <= cand.Height {
					out.Track.Crop = cc
					out.Decisions = append(out.Decisions,
						Decision{Field: "crop", Winner: cand.Source, Loser: crop.Source,
							TrackID: cand.TrackID, Message: "crop: higher-priority candidate rejected as out-of-bounds"})
					break
				}
			}
		} else {
			out.Track.Crop = c
			out.Decisions = append(out.Decisions, decisionsFor("crop", records, crop.Source)...)
		}
	}

	color := adjudicate(p.ColorPriority, records, func(t TrackMeta) bool { return t.Color != nil })
	if color != nil && color.Color != nil {
		if conflict := findColorConflict(records, color.Color); conflict != "" {
			diag.Add(model.Diagnostic{Kind: model.KindColorConflict, Severity: model.SeverityWarning,
				Track: color.TrackID, AtTicks: model.NoTimestamp, Recoverable: true,
				Message: conflict + fmt.Sprintf("; winner=%s", color.Source)})
		}
		out.Track.Color = color.Color
		out.Decisions = append(out.Decisions, decisionsFor("color", records, color.Source)...)
	}
	return out, diag
}

// pickBase chooses the highest-priority record as the merge base so identity
// fields come from a deterministic source.
func pickBase(records []TrackMeta) TrackMeta {
	base := records[0]
	for _, r := range records[1:] {
		if r.Source > base.Source {
			base = r
		}
	}
	return base
}

// adjudicate returns the winning record for one field under the priority table.
func adjudicate(table map[string]int, records []TrackMeta, present func(TrackMeta) bool) *TrackMeta {
	ranked := rankedRecords(table, records, present)
	if len(ranked) == 0 {
		return nil
	}
	return &ranked[0]
}

func policyRank(table map[string]int, source string) int {
	if r, ok := table[source]; ok {
		return r
	}
	return 0
}

// rankedRecords returns field-bearing records in descending policy rank.
func rankedRecords(table map[string]int, records []TrackMeta, present func(TrackMeta) bool) []TrackMeta {
	var out []TrackMeta
	for _, r := range records {
		if present(r) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return policyRank(table, out[i].Source) > policyRank(table, out[j].Source)
	})
	return out
}

func decisionsFor(field string, records []TrackMeta, winner string) []Decision {
	var out []Decision
	for _, r := range records {
		if r.Source == winner {
			continue
		}
		switch field {
		case "rotation":
			if !r.HasRot {
				continue
			}
		case "crop":
			if r.Crop == nil {
				continue
			}
		case "color":
			if r.Color == nil {
				continue
			}
		}
		out = append(out, Decision{Field: field, Winner: winner, Loser: r.Source,
			TrackID: r.TrackID, Message: fmt.Sprintf("%s: %s overrides %s", field, winner, r.Source)})
	}
	return out
}

func findColorConflict(records []TrackMeta, winner *ColorDesc) string {
	for _, r := range records {
		if r.Color == nil {
			continue
		}
		c := r.Color
		if c.Primaries != winner.Primaries || c.Transfer != winner.Transfer ||
			c.Matrix != winner.Matrix || c.FullRange != winner.FullRange {
			return fmt.Sprintf("color conflict from %s: nclx(%d,%d,%d,full=%v) vs winner nclx(%d,%d,%d,full=%v)",
				r.Source, c.Primaries, c.Transfer, c.Matrix, c.FullRange,
				winner.Primaries, winner.Transfer, winner.Matrix, winner.FullRange)
		}
	}
	return ""
}

// GroupByTrackID groups normalized records that describe the same logical track.
func GroupByTrackID(results ...*ParseResult) map[string][]TrackMeta {
	out := map[string][]TrackMeta{}
	for _, res := range results {
		if res == nil {
			continue
		}
		for _, t := range res.Tracks {
			out[t.TrackID] = append(out[t.TrackID], t)
		}
	}
	return out
}
