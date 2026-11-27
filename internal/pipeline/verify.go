package pipeline

import (
	"sort"

	"tsqc/internal/clock"
	"tsqc/internal/ts"
)

func VerifySegment(seg *Segment) error {
	pkts, trunc, err := ts.SplitPackets(seg.Bytes)
	if err != nil && trunc == 0 {
		return err
	}
	d := ts.NewDemux()
	var aus []ts.AccessUnit
	for _, raw := range pkts {
		got, perr := d.PushPacket(raw)
		aus = append(aus, got...)
		if perr != nil && !clock.IsRecoverable(perr) {
			return perr
		}
	}
	aus = append(aus, d.Flush()...)
	if len(aus) == 0 {
		return clock.FatalError(clock.CodeUnrecoverable, "pipeline.VerifySegment", "segment %d reparse yielded no access units", seg.Index)
	}
	if d.Tables().PAT() == nil || d.Tables().ActivePMT() == nil {
		return clock.FatalError(clock.CodePSIInvalid, "pipeline.VerifySegment", "segment %d has no PAT/PMT", seg.Index)
	}
	ordered := make([]ts.AccessUnit, len(aus))
	copy(ordered, aus)
	sort.SliceStable(ordered, func(i, j int) bool {
		return auLess(&ordered[i], &ordered[j])
	})
	aus = ordered
	chk := &TimestampCheck{}
	sem := newSemanticHash()
	videoAUs := 0
	var firstVideo *ts.AccessUnit
	for i := range aus {
		au := &aus[i]
		if au.Kind == ts.KindVideo {
			videoAUs++
			if videoAUs == 1 {
				if !au.Keyframe {
					return clock.FatalError(clock.CodeUnrecoverable, "pipeline.VerifySegment", "segment %d first video AU is not IDR", seg.Index)
				}
				cp := *au
				firstVideo = &cp
				if au.HasPTS && seg.StartPTS >= 0 {
					wrapped := seg.StartPTS % clock.Wrap
					if au.PTS%clock.Wrap != wrapped {
						return clock.FatalError(clock.CodeDigestMismatch, "pipeline.VerifySegment", "segment %d start PTS %d != %d", seg.Index, au.PTS, wrapped)
					}
				}
			}
		}
		sem.add(au)
		if au.Unrecoverable {
			return clock.FatalError(clock.CodeUnrecoverable, "pipeline.VerifySegment", "segment %d reparse contains a damaged AU", seg.Index)
		}
		if e := chk.Feed(*au); e != nil && !clock.IsRecoverable(e) {
			return e
		}
	}
	if firstVideo == nil {
		return clock.FatalError(clock.CodeUnrecoverable, "pipeline.VerifySegment", "segment %d has no video", seg.Index)
	}
	if videoAUs != seg.VideoAUs {
		return clock.FatalError(clock.CodeDigestMismatch, "pipeline.VerifySegment", "segment %d video AU count %d != %d", seg.Index, videoAUs, seg.VideoAUs)
	}
	if seg.ContentSHA != "" && sem.sum() != seg.ContentSHA {
		return clock.FatalError(clock.CodeDigestMismatch, "pipeline.VerifySegment", "segment %d content digest mismatch", seg.Index)
	}
	return nil
}
