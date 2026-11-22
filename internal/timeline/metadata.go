package timeline

import (
	"fmt"

	"avscan/internal/model"
)

// ReferenceAudio is the normalization target derived from the first audio
// track that parses successfully.
type ReferenceAudio struct {
	Channels   uint16
	SampleRate uint32 // integer Hz
}

// AudioTrackReport records original values and the chosen strategy per track.
type AudioTrackReport struct {
	TrackID           uint32
	OriginalChannels  uint16
	OriginalRateFixed uint32 // 16.16 fixed point as stored in the container
	OriginalRateHz    uint32
	AdoptedChannels   uint16
	AdoptedRateHz     uint32
	Strategy          string
	Unparseable       bool
}

// ColourReport keeps per-track colour metadata with separate missing vs
// conflict counters. Source samples are never rewritten.
type ColourReport struct {
	TrackID             uint32
	HasColr             bool
	MissingFields       int
	ConflictingFields   int
	Original            model.VideoMeta
	FieldConflicts      []string
}

// MetadataReport aggregates normalization results across the timeline.
type MetadataReport struct {
	Audio             []AudioTrackReport
	Colour            []ColourReport
	MissingColour     int // total missing colour fields across tracks
	ConflictColour    int // total conflicting colour fields across tracks
	UnparseableTracks int
}

// NormalizeAudio picks the first parseable audio track as reference and
// describes every other track against it without altering source data.
func NormalizeAudio(tl *model.Timeline) (MetadataReport, *ReferenceAudio) {
	var rep MetadataReport
	var ref *ReferenceAudio
	for _, tr := range tl.Tracks {
		if tr.Kind != model.KindAudio {
			continue
		}
		r := AudioTrackReport{TrackID: tr.ID}
		if !tr.Audio.Present || tr.Timescale == 0 {
			r.Unparseable = true
			rep.UnparseableTracks++
			rep.Audio = append(rep.Audio, r)
			continue
		}
		r.OriginalChannels = tr.Audio.ChannelCount
		r.OriginalRateFixed = tr.Audio.SampleRate
		r.OriginalRateHz = tr.Audio.SampleRate >> 16
		if ref == nil {
			ref = &ReferenceAudio{Channels: r.OriginalChannels, SampleRate: r.OriginalRateHz}
			r.AdoptedChannels = ref.Channels
			r.AdoptedRateHz = ref.SampleRate
			r.Strategy = "reference"
		} else {
			r.AdoptedChannels = ref.Channels
			r.AdoptedRateHz = ref.SampleRate
			r.Strategy = describeAudioStrategy(r.OriginalChannels, r.OriginalRateHz, *ref)
		}
		rep.Audio = append(rep.Audio, r)
	}
	return rep, ref
}

// describeAudioStrategy names the resampling/down-mix implied by adopting the
// reference. It only documents the plan; source samples stay untouched.
func describeAudioStrategy(channels uint16, rateHz uint32, ref ReferenceAudio) string {
	switch {
	case channels == ref.Channels && uint32(rateHz) == ref.SampleRate:
		return "match_reference"
	case channels != ref.Channels && uint32(rateHz) != ref.SampleRate:
		return fmt.Sprintf("downmix_to_%d_channels_and_resample_to_%d_hz",
			ref.Channels, ref.SampleRate)
	case channels != ref.Channels:
		return fmt.Sprintf("downmix_to_%d_channels", ref.Channels)
	default:
		return fmt.Sprintf("resample_to_%d_hz", ref.SampleRate)
	}
}

// NormalizeColour compares every video track's colour metadata against the
// first track carrying colr. Missing fields and conflicts are counted apart.
func NormalizeColour(tl *model.Timeline) MetadataReport {
	var rep MetadataReport
	var ref *model.VideoMeta
	for _, tr := range tl.Tracks {
		if tr.Kind != model.KindVideo {
			continue
		}
		cr := ColourReport{TrackID: tr.ID, HasColr: tr.Video.HasColr, Original: tr.Video}
		if !tr.Video.HasColr {
			cr.MissingFields = 4 // primaries, transfer, matrix, range flag
			rep.MissingColour += cr.MissingFields
			rep.Colour = append(rep.Colour, cr)
			continue
		}
		if ref == nil {
			ref = &tr.Video
			rep.Colour = append(rep.Colour, cr)
			continue
		}
		cr.FieldConflicts, cr.ConflictingFields = diffColour(*ref, tr.Video)
		rep.ConflictColour += cr.ConflictingFields
		rep.Colour = append(rep.Colour, cr)
	}
	return rep
}

func diffColour(ref, cur model.VideoMeta) ([]string, int) {
	var conflicts []string
	check := func(name string, a, b uint16) {
		if a != b {
			conflicts = append(conflicts, fmt.Sprintf("%s: ref=%d cur=%d", name, a, b))
		}
	}
	check("primaries", ref.ColourPrimaries, cur.ColourPrimaries)
	check("transfer", ref.TransferCharacteristics, cur.TransferCharacteristics)
	check("matrix", ref.MatrixCoefficients, cur.MatrixCoefficients)
	if ref.FullRange != cur.FullRange {
		conflicts = append(conflicts, fmt.Sprintf("full_range: ref=%v cur=%v", ref.FullRange, cur.FullRange))
	}
	return conflicts, len(conflicts)
}
