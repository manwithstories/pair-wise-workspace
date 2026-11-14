package pack

import (
	"sort"

	"dashpack/internal/align"
	"dashpack/internal/mpd"
)

// Addressing modes for the emitted SegmentTemplate@media.
const (
	AddrNumber = "number"
	AddrTime   = "time"
)

// BuildManifest renders an aligned ladder into an MPD model: one
// AdaptationSet per tier, Representations ordered by ascending bandwidth,
// every timeline expressed in the common timescale on the shared grid.
func BuildManifest(res *align.Result, addrMode string) (*mpd.MPD, error) {
	reps := make([]align.AlignedRep, len(res.Reps))
	copy(reps, res.Reps)
	sort.SliceStable(reps, func(i, j int) bool {
		return reps[i].Track.Bandwidth < reps[j].Track.Bandwidth
	})
	media := "$Number%05d$.m4s"
	if addrMode == AddrTime {
		media = "$Time%09d$.m4s"
	}
	sets := make([]mpd.AdaptationSet, 0, len(reps))
	for i, r := range reps {
		tl, err := mpd.BuildTimeline(r.Cuts)
		if err != nil {
			return nil, err
		}
		st := mpd.SegmentTemplate{
			Timescale:              align.CommonTimescale,
			PresentationTimeOffset: r.PTO,
			Media:                  media,
			SegmentTimeline:        tl,
		}
		if addrMode == AddrNumber {
			sn := r.Track.StartNumber
			st.StartNumber = &sn
		}
		rep := mpd.Representation{
			ID:              r.Track.ID,
			Bandwidth:       r.Track.Bandwidth,
			Width:           r.Track.Width,
			Height:          r.Track.Height,
			BaseURL:         r.Track.ID + "/",
			SegmentTemplate: st,
		}
		sets = append(sets, mpd.AdaptationSet{
			ID:              i + 1,
			MimeType:        "video/mp4",
			ContentType:     "video",
			Representations: []mpd.Representation{rep},
		})
	}
	durSec := (res.DurationCommon + align.CommonTimescale/2) / align.CommonTimescale
	return &mpd.MPD{
		Profiles:                  "urn:mpeg:dash:profile:isoff-live:2011",
		Type:                      "static",
		MediaPresentationDuration: mpd.FormatDuration(durSec),
		MinBufferTime:             "PT2S",
		Periods: []mpd.Period{{
			ID:             "p0",
			Start:          "PT0S",
			AdaptationSets: sets,
		}},
	}, nil
}
