package analyze

import (
	"sort"

	"mediascan/internal/media"
	"mediascan/internal/parser"
)

// Keyframe is one sync sample in native and unified time bases.
type Keyframe struct {
	SampleNumber   int    `json:"sample_number"`
	Ticks          int64  `json:"ticks"`
	Timescale      uint32 `json:"timescale"`
	UnifiedTicks   int64  `json:"unified_ticks"`
	UnifiedValid   bool   `json:"unified_valid"`
	FileByteOffset uint64 `json:"file_byte_offset,omitempty"`
}

// KeyframeInterval is the exact duration between consecutive keyframes.
type KeyframeInterval struct {
	FromSample   int        `json:"from_sample"`
	ToSample     int        `json:"to_sample"`
	Start        media.Time `json:"start"`
	End          media.Time `json:"end"`
	Ticks        int64      `json:"ticks"`
	Timescale    uint32     `json:"timescale"`
	UnifiedTicks int64      `json:"unified_ticks"`
	UnifiedValid bool       `json:"unified_valid"`
	ErrorTicks   int64      `json:"rounding_error_ticks,omitempty"`
}

// KeyframeReport contains sorted keyframes and intervals for one track.
type KeyframeReport struct {
	FileIndex int                `json:"file_index"`
	File      string             `json:"file"`
	TrackID   uint32             `json:"track_id"`
	Timescale uint32             `json:"timescale"`
	Keyframes []Keyframe         `json:"keyframes"`
	Intervals []KeyframeInterval `json:"intervals"`
}

// AnalyzeKeyframes extracts ordinary stss and fragmented trun sync samples.
func AnalyzeKeyframes(pf *parser.File, fileIndex int) []KeyframeReport {
	out := make([]KeyframeReport, 0, len(pf.Tracks))
	for _, t := range pf.Tracks {
		out = append(out, keyframesForTrack(pf, t, fileIndex))
	}
	return out
}

func keyframesForTrack(pf *parser.File, t *parser.Track, fileIndex int) KeyframeReport {
	r := KeyframeReport{
		FileIndex: fileIndex,
		File:      pf.Source.Name,
		TrackID:   t.TrackID,
		Timescale: t.Timescale,
	}

	type kt struct {
		number int
		ticks  int64
		offset uint64
	}
	var points []kt

	for _, s := range t.Samples {
		if s.Keyframe {
			points = append(points, kt{number: s.Number, ticks: s.DecodeTime, offset: s.Offset})
		}
	}
	for _, f := range t.Fragments {
		pos := f.BaseTime
		for _, s := range f.Samples {
			if s.Keyframe {
				points = append(points, kt{number: s.Number, ticks: pos})
			}
			pos += int64(s.Duration)
		}
	}

	sort.SliceStable(points, func(i, j int) bool {
		if points[i].ticks != points[j].ticks {
			return points[i].ticks < points[j].ticks
		}
		return points[i].number < points[j].number
	})

	for _, p := range points {
		u, ok := media.ScaleTicks(p.ticks, t.Timescale, media.UnifiedTimescale)
		r.Keyframes = append(r.Keyframes, Keyframe{
			SampleNumber:   p.number,
			Ticks:          p.ticks,
			Timescale:      t.Timescale,
			UnifiedTicks:   u,
			UnifiedValid:   ok,
			FileByteOffset: p.offset,
		})
	}
	for i := 1; i < len(r.Keyframes); i++ {
		a, b := r.Keyframes[i-1], r.Keyframes[i]
		start := media.NewTime(a.Ticks, t.Timescale)
		end := media.NewTime(b.Ticks, t.Timescale)
		u, ok := media.ScaleTicks(b.Ticks-a.Ticks, t.Timescale, media.UnifiedTimescale)
		r.Intervals = append(r.Intervals, KeyframeInterval{
			FromSample:   a.SampleNumber,
			ToSample:     b.SampleNumber,
			Start:        start,
			End:          end,
			Ticks:        b.Ticks - a.Ticks,
			Timescale:    t.Timescale,
			UnifiedTicks: u,
			UnifiedValid: ok,
		})
	}
	return r
}

// MaxKeyframeTickError compares observed keyframe ticks with expected ticks.
// It is used by the demo's zero-tolerance/one-tick acceptance assertion.
func MaxKeyframeTickError(actual []Keyframe, expected []int64) int64 {
	var max int64
	for i := 0; i < len(actual) && i < len(expected); i++ {
		d := actual[i].Ticks - expected[i]
		if d < 0 {
			d = -d
		}
		if d > max {
			max = d
		}
	}
	return max
}
