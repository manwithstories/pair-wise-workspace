package verify

import (
	"fmt"
	"time"

	"m3u8ctl/internal/ts"
)

type timelineStart struct {
	sequence int64
	ticks    int64
}

func addCrossVariantFailures(data []*variantData, tolerance time.Duration) {
	const skewTolerance = 250 * time.Millisecond
	if len(data) < 2 {
		return
	}
	maxSegments := 0
	for _, item := range data {
		if item.report.SegmentsExpected > maxSegments {
			maxSegments = item.report.SegmentsExpected
		}
	}
	for _, item := range data {
		if item.media != nil && item.report.SegmentsExpected != maxSegments {
			addFailure(item, "SEGMENT_COUNT_MISMATCH", item.media.MediaSequence, "index.m3u8",
				"variant has a different segment count", fmt.Sprintf("%d", maxSegments), fmt.Sprintf("%d", item.report.SegmentsExpected))
		}
	}
	baseline := timelineStarts(data[0])
	if len(baseline) < 2 {
		return
	}
	for _, item := range data[1:] {
		current := timelineStarts(item)
		limit := len(baseline)
		if len(current) < limit {
			limit = len(current)
		}
		for i := 1; i < limit; i++ {
			baseRel := ts.TicksToDuration(baseline[i].ticks - baseline[0].ticks)
			curRel := ts.TicksToDuration(current[i].ticks - current[0].ticks)
			diff := absDuration(baseRel - curRel)
			if diff > skewTolerance && diff > tolerance {
				addFailure(item, "VARIANT_SKEW", current[i].sequence, "index.m3u8",
					"cumulative variant start differs by more than 250ms", "<=250ms", diff.String())
				break
			}
		}
	}
}

func timelineStarts(item *variantData) []timelineStart {
	starts := make([]timelineStart, 0, len(item.segments))
	for _, segment := range item.segments {
		if !segment.validTimeline {
			continue
		}
		starts = append(starts, timelineStart{sequence: segment.sequence, ticks: segment.probe.FirstPCR})
	}
	return starts
}
