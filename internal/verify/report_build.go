package verify

import (
	"fmt"

	"m3u8ctl/internal/segment"
)

func (checker *Checker) buildReport(duplicates int) *Report {
	report := &Report{
		TargetDuration: int(checker.index.SlotMS / 1000),
		DriftMaxMS:     checker.maxDrift(),
		RoundTripOK:    checker.roundTripOK,
		Duplicates:     duplicates,
		Failures:       checker.failures,
		RenditionStats: make([]RenditionReport, 0, len(checker.index.Ordered)),
	}
	if report.Failures == nil {
		report.Failures = []FailureDetail{}
	}
	var gapTotal int64
	var segmentTotal int
	var present int
	var firstPresent *segment.RenditionIndex
	maxSeq := int64(-1)
	for _, rIndex := range checker.index.Ordered {
		if len(rIndex.Slots) == 0 {
			continue
		}
		present++
		if firstPresent == nil {
			firstPresent = rIndex
		}
		media := checker.medias[rIndex.Rendition]
		gaps := rIndex.GapCount()
		gapTotal += gaps
		segments := len(rIndex.Slots)
		segmentTotal += segments
		stats := RenditionReport{
			Rendition:    rIndex.Rendition,
			Name:         rIndex.Profile.Name,
			BitrateBps:   rIndex.Profile.BitrateBps,
			Resolution:   fmt.Sprintf("%dx%d", rIndex.Profile.Width, rIndex.Profile.Height),
			Segments:     segments,
			Gaps:         gaps,
			GapIntervals: len(rIndex.Gaps),
			MaxDriftMS:   rIndex.MaxDriftMS,
		}
		if media != nil && len(media.Entries) > 0 {
			stats.WindowStart = media.MediaSequence
			stats.WindowEnd = media.Entries[len(media.Entries)-1].Seq
		}
		report.RenditionStats = append(report.RenditionStats, stats)
		if rIndex.MaxSeq > maxSeq {
			maxSeq = rIndex.MaxSeq
		}
	}
	report.Renditions = present
	report.Segments = segmentTotal / max(1, present)
	report.Gaps = gapTotal / int64(max(1, present))
	if firstPresent != nil {
		report.GapIntervals = firstPresent.GapIntervalsCount()
	}
	if maxSeq >= 0 {
		report.DurationS = float64((maxSeq+1)*checker.index.SlotMS) / 1000
	}
	return report
}

func (checker *Checker) maxDrift() int64 {
	var maximum int64
	for _, rIndex := range checker.index.Ordered {
		if rIndex.MaxDriftMS > maximum {
			maximum = rIndex.MaxDriftMS
		}
	}
	return maximum
}
