package timeline

import (
	"fmt"
	"sort"
)

type Segment struct {
	Start     int64
	Duration  int64
	ByteStart int64
	ByteEnd   int64
	Path      string
}

type Track struct {
	ID        string
	Kind      string
	Timescale int64
	Segments  []Segment
}

type Config struct {
	TargetTimescale int64
	GridMillis      int64
	ToleranceMillis int64
}

type Issue struct {
	Code      string
	TrackID   string
	Index     int
	Amount    int64
	Timescale int64
}

type Report struct {
	TargetTimescale     int64
	GridTicks           int64
	ToleranceTicks      int64
	MaxDeviationTicks   int64
	PresentationOffsets map[string]int64
	Gaps                int
	Overlaps            int
	Issues              []Issue
}

func Align(tracks []Track, cfg Config) (*Report, error) {
	if cfg.TargetTimescale <= 0 || cfg.GridMillis <= 0 || cfg.ToleranceMillis < 0 {
		return nil, fmt.Errorf("invalid alignment configuration")
	}
	grid, err := MillisToTicks(cfg.GridMillis, cfg.TargetTimescale)
	if err != nil {
		return nil, err
	}
	tolerance, err := MillisToTicks(cfg.ToleranceMillis, cfg.TargetTimescale)
	if err != nil {
		return nil, err
	}
	if grid <= 0 {
		return nil, fmt.Errorf("segment grid rounds to zero ticks")
	}
	report := &Report{
		TargetTimescale:     cfg.TargetTimescale,
		GridTicks:           grid,
		ToleranceTicks:      tolerance,
		PresentationOffsets: make(map[string]int64, len(tracks)),
	}
	for _, track := range tracks {
		if err := alignTrack(report, track, grid, tolerance); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(report.Issues, func(i, j int) bool {
		if report.Issues[i].TrackID != report.Issues[j].TrackID {
			return report.Issues[i].TrackID < report.Issues[j].TrackID
		}
		return report.Issues[i].Index < report.Issues[j].Index
	})
	return report, nil
}

func alignTrack(report *Report, track Track, grid, tolerance int64) error {
	if track.ID == "" || track.Timescale <= 0 {
		return fmt.Errorf("track %q has invalid id or timescale", track.ID)
	}
	if len(track.Segments) == 0 {
		return fmt.Errorf("track %q has no segments", track.ID)
	}
	type converted struct {
		start    int64
		duration int64
	}
	convertedSegments := make([]converted, len(track.Segments))
	for i, segment := range track.Segments {
		start, err := ScaleTicks(segment.Start, track.Timescale, report.TargetTimescale)
		if err != nil {
			return fmt.Errorf("track %q segment %d start: %w", track.ID, i, err)
		}
		duration, err := ScaleTicks(segment.Duration, track.Timescale, report.TargetTimescale)
		if err != nil {
			return fmt.Errorf("track %q segment %d duration: %w", track.ID, i, err)
		}
		convertedSegments[i] = converted{start: start, duration: duration}
	}
	offset := convertedSegments[0].start
	report.PresentationOffsets[track.ID] = offset
	for i, segment := range convertedSegments {
		if int64(i) > int64Max/grid {
			return fmt.Errorf("track %q grid position overflow", track.ID)
		}
		expected := int64(i) * grid
		delta := (segment.start - offset) - expected
		if Abs(delta) > report.MaxDeviationTicks {
			report.MaxDeviationTicks = Abs(delta)
		}
		if i == 0 {
			continue
		}
		previous := convertedSegments[i-1]
		boundary := segment.start - (previous.start + previous.duration)
		switch {
		case boundary > tolerance:
			report.Gaps++
			report.Issues = append(report.Issues, Issue{
				Code: "ALIGN_GAP", TrackID: track.ID, Index: i,
				Amount: boundary, Timescale: report.TargetTimescale,
			})
		case boundary < -tolerance:
			report.Overlaps++
			report.Issues = append(report.Issues, Issue{
				Code: "ALIGN_OVERLAP", TrackID: track.ID, Index: i,
				Amount: -boundary, Timescale: report.TargetTimescale,
			})
		}
	}
	return nil
}

const int64Max = int64(^uint64(0) >> 1)
