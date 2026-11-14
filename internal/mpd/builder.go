package mpd

import (
	"fmt"
	"sort"
	"strconv"

	"dashpack/internal/align"
	"dashpack/internal/mp4"
)

// Media and initialization template names shared by builder and packer.
const (
	MediaTemplate = "seg-$RepresentationID$-$Time$.m4s"
	InitTemplate  = "init-$RepresentationID$.mp4"
)

// Build assembles the MPD model from scanned timelines and the alignment
// result. Representations are sorted by bandwidth for stable output.
func Build(metas []align.RepMeta, tls map[string]*mp4.Timeline, al *align.Result) (*MPD, error) {
	if len(metas) == 0 {
		return nil, fmt.Errorf("mpd: no representations to build")
	}
	sorted := make([]align.RepMeta, len(metas))
	copy(sorted, metas)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Bandwidth != sorted[j].Bandwidth {
			return sorted[i].Bandwidth < sorted[j].Bandwidth
		}
		return sorted[i].ID < sorted[j].ID
	})
	pto := make(map[string]uint64, len(al.Reps))
	for _, r := range al.Reps {
		pto[r.ID] = r.PresentationTimeOffset
	}
	set := AdaptationSet{ID: 1, ContentType: "video"}
	var maxDurationMs float64
	for _, meta := range sorted {
		tl := tls[meta.ID]
		if tl == nil || len(tl.Segments) == 0 {
			return nil, fmt.Errorf("mpd: representation %q has no scanned timeline", meta.ID)
		}
		rep := Representation{
			ID:        meta.ID,
			Bandwidth: meta.Bandwidth,
			SegmentTemplate: SegmentTemplate{
				Timescale:              uint64(tl.Timescale),
				Duration:               tl.NominalDuration(),
				StartNumber:            uint64(tl.Segments[0].Number),
				PresentationTimeOffset: pto[meta.ID],
				Media:                  MediaTemplate,
				Initialization:         InitTemplate,
				SegmentTimeline:        buildTimeline(tl),
			},
		}
		if end := msOf(tl.End()-pto[meta.ID], tl.Timescale); end > maxDurationMs {
			maxDurationMs = end
		}
		set.Representations = append(set.Representations, rep)
	}
	dur := isoDuration(maxDurationMs)
	return &MPD{
		XMLNS:                     Namespace,
		Profiles:                  Profiles,
		Type:                      "static",
		MediaPresentationDuration: dur,
		MinBufferTime:             "PT2S",
		Period: Period{
			ID:             "p0",
			Duration:       dur,
			AdaptationSets: []AdaptationSet{set},
		},
	}, nil
}

// buildTimeline converts scanned segments into S entries, compressing runs
// of equal duration with the repeat attribute. The t attribute is emitted
// on the first entry and whenever a time jump (gap) occurs.
func buildTimeline(tl *mp4.Timeline) *SegmentTimeline {
	st := &SegmentTimeline{}
	var expect uint64
	for i, seg := range tl.Segments {
		if i == 0 || seg.Start != expect {
			st.S = append(st.S, S{T: seg.Start, D: seg.Duration})
		} else if n := len(st.S); n > 0 && st.S[n-1].D == seg.Duration {
			st.S[n-1].R++
		} else {
			st.S = append(st.S, S{T: seg.Start, D: seg.Duration})
		}
		expect = seg.Start + seg.Duration
	}
	return st
}

func msOf(v uint64, timescale uint32) float64 {
	if timescale == 0 {
		return 0
	}
	return float64(v) * 1000 / float64(timescale)
}

// isoDuration renders fractional milliseconds as an ISO 8601 duration.
func isoDuration(ms float64) string {
	total := int64(ms + 0.5)
	sec := total / 1000
	rem := total % 1000
	if rem == 0 {
		return "PT" + strconv.FormatInt(sec, 10) + "S"
	}
	return "PT" + strconv.FormatInt(sec, 10) + "." + fmt.Sprintf("%03d", rem) + "S"
}
