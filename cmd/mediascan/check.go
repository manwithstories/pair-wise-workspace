package main

import (
	"fmt"

	"mediascan/internal/analyze"
	"mediascan/internal/media"
	"mediascan/internal/report"
	"mediascan/internal/samplegen"
)

func verifyDemoExpectations(set *samplegen.FixtureSet, rep *report.Report) error {
	if rep.Metrics.Files != 6 || rep.Metrics.Tracks != 12 || rep.Metrics.CorruptRegions != 4 {
		return fmt.Errorf("unexpected counts files=%d tracks=%d corrupt=%d", rep.Metrics.Files, rep.Metrics.Tracks, rep.Metrics.CorruptRegions)
	}
	inputByIndex := map[int]report.Input{}
	for _, in := range rep.Inputs {
		inputByIndex[in.Index] = in
	}

	maxOffset := int64(0)
	for _, want := range set.Regions {
		in, ok := inputByIndex[want.FileIndex]
		if !ok {
			return fmt.Errorf("missing input index %d", want.FileIndex)
		}
		best := int64(1 << 62)
		var found bool
		for _, f := range rep.Faults {
			if f.Code != want.Code || f.FileSHA256 != in.SHA256 {
				continue
			}
			if want.TrackID != 0 && f.TrackID != want.TrackID {
				continue
			}
			d := f.ByteOffset - want.Offset
			if d < 0 {
				d = -d
			}
			if d < best {
				best, found = d, true
			}
		}
		if !found {
			return fmt.Errorf("expected fault %s file=%d track=%d offset=%d not found", want.Code, want.FileIndex, want.TrackID, want.Offset)
		}
		if best > maxOffset {
			maxOffset = best
		}
	}
	if maxOffset != 0 {
		return fmt.Errorf("corruption offset error %d bytes", maxOffset)
	}
	rep.Metrics.OffsetErrorMaxBytes = maxOffset

	maxTick := int64(0)
	for _, want := range set.Keyframes {
		var tr *report.TrackReport
		for i := range rep.Tracks {
			if rep.Tracks[i].FileIndex == want.FileIndex && rep.Tracks[i].TrackID == want.TrackID {
				tr = &rep.Tracks[i]
				break
			}
		}
		if tr == nil {
			return fmt.Errorf("missing track file=%d track=%d", want.FileIndex, want.TrackID)
		}
		if d := analyze.MaxKeyframeTickError(tr.Keyframes, want.Ticks); d > maxTick {
			maxTick = d
		}
	}
	if maxTick > 1 {
		return fmt.Errorf("keyframe time error %d ticks > 1", maxTick)
	}
	rep.Metrics.KeyframeTimeErrorMaxTicks = maxTick

	for _, f := range rep.Faults {
		if f.Severity == media.SeverityError && f.FileSHA256 == "" {
			return fmt.Errorf("fault %s is missing file hash", f.Code)
		}
	}
	return nil
}
