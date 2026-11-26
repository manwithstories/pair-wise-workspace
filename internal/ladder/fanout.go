// Package ladder builds multi-bitrate representations on common keyframe edges.
package ladder

import (
	"mediad/internal/media"
)

// Level is one encoded representation.
type Level struct {
	Name        string `json:"name"`
	Width       uint16 `json:"width"`
	Height      uint16 `json:"height"`
	BitrateKbps int    `json:"bitrate_kbps"`
}

// DefaultLevels is a small deterministic mobile ladder.
func DefaultLevels() []Level {
	return []Level{
		{Name: "240p", Width: 426, Height: 240, BitrateKbps: 400},
		{Name: "360p", Width: 640, Height: 360, BitrateKbps: 900},
		{Name: "720p", Width: 1280, Height: 720, BitrateKbps: 2500},
		{Name: "1080p", Width: 1920, Height: 1080, BitrateKbps: 5000},
	}
}

// Boundary is a common IDR/keyframe switch point.
type Boundary struct {
	SampleNumber uint32 `json:"sample_number"`
	TimeNS       int64  `json:"time_ns"`
	PTSTicks     int64  `json:"pts_ticks"`
}

// Rendition is one bitrate track's boundary set.
type Rendition struct {
	Level      Level      `json:"level"`
	TrackID    uint32     `json:"track_id"`
	Boundaries []Boundary `json:"boundaries"`
}

// Ladder keeps all representations aligned to identical boundaries.
type Ladder struct {
	FrameDurationNS int64        `json:"frame_duration_ns"`
	Renditions      []*Rendition `json:"renditions"`
}

// Build creates rendition boundary lists from sync samples. Every rendition gets
// the same source sample numbers and times; only encoding parameters differ.
func Build(video *media.Track, levels []Level) (*Ladder, []*media.Fault) {
	var faults []*media.Fault
	if video == nil || video.Kind != media.KindVideo {
		return nil, []*media.Fault{media.NewFault(media.CodeInvalidInput, "ladder requires a video track").Fatal()}
	}
	if len(levels) == 0 {
		levels = DefaultLevels()
	}
	clock, err := video.Clock()
	if err != nil {
		return nil, []*media.Fault{media.NewFault(media.CodeInvalidInput, "%v", err).WithTrack(video.ID).Fatal()}
	}
	var boundaries []Boundary
	for _, s := range video.Samples {
		if !s.Sync {
			continue
		}
		ns, err := clock.TicksToNanoseconds(s.PTS)
		if err != nil {
			faults = append(faults, media.NewFault(media.CodeInvalidInput, "boundary time overflow: %v", err).WithTrack(video.ID).Fatal())
			continue
		}
		boundaries = append(boundaries, Boundary{SampleNumber: s.Index, TimeNS: ns, PTSTicks: s.PTS})
	}
	if len(boundaries) == 0 {
		return nil, append(faults, media.NewFault(media.CodeMissingIndex, "no common keyframe boundaries").WithTrack(video.ID).WithBox("stss").Fatal())
	}
	frameNS, err := video.FrameDurationNS()
	if err != nil {
		faults = append(faults, media.NewFault(media.CodeInvalidInput, "%v", err).WithTrack(video.ID).Fatal())
	}
	l := &Ladder{FrameDurationNS: frameNS}
	for i, level := range levels {
		l.Renditions = append(l.Renditions, &Rendition{
			Level: level, TrackID: 100 + uint32(i), Boundaries: append([]Boundary(nil), boundaries...),
		})
	}
	return l, faults
}

// SwitchResult reports cross-rendition alignment around an ABR switch.
type SwitchResult struct {
	From          string `json:"from"`
	To            string `json:"to"`
	SampleNumber  uint32 `json:"sample_number"`
	FromTimeNS    int64  `json:"from_time_ns"`
	ToTimeNS      int64  `json:"to_time_ns"`
	DeltaNS       int64  `json:"delta_ns"`
	OneFrameMaxNS int64  `json:"one_frame_max_ns"`
	Aligned       bool   `json:"aligned"`
}

// CheckSwitch verifies that two renditions expose the selected sample as the
// same common keyframe boundary and differ by no more than one frame.
func (l *Ladder) CheckSwitch(fromIdx, toIdx int, sampleNumber uint32) (SwitchResult, *media.Fault) {
	if l == nil || fromIdx < 0 || toIdx < 0 || fromIdx >= len(l.Renditions) || toIdx >= len(l.Renditions) {
		return SwitchResult{}, media.NewFault(media.CodeBoundaryMismatch, "rendition index out of range").Fatal()
	}
	from := l.Renditions[fromIdx]
	to := l.Renditions[toIdx]
	a, oka := boundaryBySample(from.Boundaries, sampleNumber)
	b, okb := boundaryBySample(to.Boundaries, sampleNumber)
	res := SwitchResult{From: from.Level.Name, To: to.Level.Name, SampleNumber: sampleNumber, OneFrameMaxNS: l.FrameDurationNS}
	if !oka || !okb {
		f := media.NewFault(media.CodeBoundaryMismatch, "switch sample %d is not a keyframe in both renditions", sampleNumber).
			WithBox("ladder")
		return res, f
	}
	res.FromTimeNS, res.ToTimeNS = a.TimeNS, b.TimeNS
	res.DeltaNS = abs64(a.TimeNS - b.TimeNS)
	// Rounding can make one frame interval appear as FrameDurationNS ± one
	// shared tick; allow a small rational conversion epsilon.
	epsilon := int64(1_000_000) // 1 ms
	res.Aligned = res.DeltaNS <= l.FrameDurationNS+epsilon
	if !res.Aligned {
		return res, media.NewFault(media.CodeBoundaryMismatch, "switch delta %dns exceeds one frame %dns", res.DeltaNS, l.FrameDurationNS).
			WithTimeSpan(a.PTSTicks, b.PTSTicks, media.Clock{SecondsPerTick: media.MustRational(1, 90_000)})
	}
	return res, nil
}

// MismatchCount checks every adjacent rendition pair at every boundary.
func (l *Ladder) MismatchCount() int {
	mismatches := 0
	if l == nil || len(l.Renditions) < 2 {
		return 0
	}
	for sampleIdx, b := range l.Renditions[0].Boundaries {
		for r := 1; r < len(l.Renditions); r++ {
			if sampleIdx >= len(l.Renditions[r].Boundaries) {
				mismatches++
				continue
			}
			other := l.Renditions[r].Boundaries[sampleIdx]
			if other.SampleNumber != b.SampleNumber || other.TimeNS != b.TimeNS {
				mismatches++
			}
		}
	}
	return mismatches
}

func boundaryBySample(bs []Boundary, n uint32) (Boundary, bool) {
	for _, b := range bs {
		if b.SampleNumber == n {
			return b, true
		}
	}
	return Boundary{}, false
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
