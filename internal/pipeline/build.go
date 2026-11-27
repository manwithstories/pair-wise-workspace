package pipeline

import (
	"sort"

	"tsqc/internal/clock"
	"tsqc/internal/ts"
)

func (s *Segmenter) Build(aus []ts.AccessUnit) ([]Segment, error) {
	if s.PCRInterval <= 0 {
		s.PCRInterval = 2
	}
	items := make([]segItem, 0, len(aus))
	for _, au := range aus {
		pts := au.PTS
		if pts < 0 {
			pts = au.DTS
		}
		items = append(items, segItem{au: au, pts: pts, kind: au.Kind})
	}
	sort.SliceStable(items, func(i, j int) bool { return itemLess(items[i], items[j]) })

	var segs []Segment
	var cur []segItem
	var startPTS int64
	var gapBack bool
	flush := func() error {
		if len(cur) == 0 {
			return nil
		}
		seg := Segment{Index: len(segs), StartPTS: startPTS, Keyframe: true, GapBack: gapBack}
		body, v, a, err := s.mux(cur, &seg)
		if err != nil {
			return err
		}
		seg.VideoAUs, seg.AudioAUs = v, a
		seg.Bytes = body
		seg.Size = len(body)
		seg.ContentSHA = semanticDigest(cur)
		seg.InputSHA = inputDigest(cur)
		seg.Digest()
		segs = append(segs, seg)
		cur = cur[:0]
		return nil
	}
	pendingGap := false
	for _, it := range items {
		if it.au.Unrecoverable {
			// A damaged access unit is never muxed. The current segment ends
			// at the last intact frame and the next IDR resumes cleanly with
			// its gap flag set.
			if len(cur) > 0 {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			pendingGap = true
			continue
		}
		if it.kind == ts.KindVideo && it.au.Keyframe {
			if len(cur) > 0 {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			startPTS = it.pts
			gapBack = pendingGap
			pendingGap = false
			cur = append(cur, it)
			continue
		}
		if len(cur) == 0 {
			// Before the first intact IDR there is nothing playable.
			continue
		}
		cur = append(cur, it)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	for i := range segs {
		if segs[i].StartPTS < 0 {
			return nil, clock.FatalError(clock.CodeUnrecoverable, "pipeline.Build", "segment %d has no anchor PTS", i)
		}
	}
	return segs, nil
}

// semanticHash canonicalises the demuxed view of a segment: per access unit
// its PID, track kind, presentation time, keyframe flag and payload digest.
// Two segmentations that survive a round trip byte-identically produce the
// same semantic digest even if mux details change.
