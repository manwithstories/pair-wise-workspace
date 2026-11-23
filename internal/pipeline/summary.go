package pipeline

import (
	"os"
	"path/filepath"

	"mediasim/internal/clock"
	"mediasim/internal/mp4"
)

// recoverySummary converts scanner output into report fields.
func recoverySummary(info *mp4.Info, rec *mp4.Recovery) (RecoveryReport, []ErrorEntry) {
	var rr RecoveryReport
	var errs []ErrorEntry
	rr.Indexed = info.Indexed
	if rec != nil {
		total, committed, phantom := rec.Counts()
		rr.ScannedSamples = total
		rr.CommittedScan = committed
		rr.Phantom = phantom
		rr.SkippedRanges = len(rec.Skipped)
		rr.Resynced = rec.Resynced
	}
	for _, g := range info.Gaps {
		errs = append(errs, ErrorEntry{
			Category: string(gapCategory(g.Kind)),
			Span:     clock.Span{Interval: clock.Interval{Start: int64(g.Samples[0]), End: int64(g.Samples[1])}}.String(),
			Recovery: string(g.Recovery),
			Message:  g.Detail,
		})
	}
	return rr, errs
}

func gapCategory(k mp4.GapKind) clock.Category {
	switch k {
	case mp4.GapMissingIndex:
		return clock.CatIndexGap
	case mp4.GapTruncatedTable:
		return clock.CatSampleTable
	case mp4.GapCorruptSample:
		return clock.CatSample
	case mp4.GapOutOfRange:
		return clock.CatContainer
	default:
		return clock.CatSampleTable
	}
}

// WriteReport writes report.json into dir and returns its path.
func WriteReport(dir string, r *Report) (string, error) {
	path := filepath.Join(dir, "report.json")
	f, err := os.Create(path)
	if err != nil {
		return "", clock.NewError(clock.CatArgument, clock.Span{}, clock.RecoveredNone, "create report", err)
	}
	defer f.Close()
	if err := r.WriteJSON(f); err != nil {
		return "", clock.NewError(clock.CatRender, clock.Span{}, clock.RecoveredNone, "write report", err)
	}
	return path, nil
}
