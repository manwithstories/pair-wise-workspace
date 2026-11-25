package pipeline

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"mediad/internal/checkpoint"
	"mediad/internal/cut"
	"mediad/internal/pack"
	"mediad/internal/timeline"
)

// ErrInterrupted is the deliberate crash point used by resume demonstrations.
var ErrInterrupted = errors.New("simulated interruption after committed segment")

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func alignmentError(spans []timeline.Span, keyIndex, auEnd int, startNS, segEndNS int64) int64 {
	expected := startNS
	for i := keyIndex; i < auEnd && i < len(spans); i++ {
		expected += spans[i].DurationNS
	}
	return abs64(segEndNS - expected)
}

func (rn *runner) commitSegment(seg cut.Segment, auEnd int) error {
	align := alignmentError(rn.spans, seg.KeyframeIndex, auEnd, seg.StartNS, seg.EndNS)
	if align > rn.alignMax {
		rn.alignMax = align
	}
	fileName := fmt.Sprintf("seg-%s-%03d.m4s", rn.opts.Codec, seg.ID)

	for _, old := range rn.journal.Segments {
		if old.ID == seg.ID {
			rn.manifestSegments = append(rn.manifestSegments, ManifestSegment{
				ID: seg.ID, File: old.File, SHA256: old.SHA256, Size: old.Size,
				StartNS: seg.StartNS, EndNS: seg.EndNS, DurationNS: seg.DurationNS,
				AUStart: seg.KeyframeIndex, AUEndExclusive: auEnd, AlignErrorNS: align,
			})
			return nil
		}
	}

	samples := make([]pack.Sample, 0, len(seg.AUIndices))
	for _, auIndex := range seg.AUIndices {
		idx := rn.stream.Indexes[auIndex]
		end := idx.Offset + int64(idx.Length)
		samples = append(samples, pack.Sample{
			Data:       rn.stream.Blob[idx.Offset:end],
			DurationNS: idx.DurationNS,
			Keyframe:   idx.Keyframe,
		})
	}
	fmp4, err := pack.BuildMedia(pack.Fragment{
		SequenceNumber: uint32(seg.ID + 1),
		StartNS:        seg.StartNS,
		Samples:        samples,
	})
	if err != nil {
		return err
	}
	fullPath, err := checkpoint.SafePath(rn.opts.OutDir, fileName)
	if err != nil {
		return err
	}
	if err := checkpoint.AtomicWriteFile(fullPath, fmp4, 0o644); err != nil {
		return timeline.Wrap(timeline.KindStorage, "pipeline.write_segment", seg.ID, -1, err)
	}
	sum := sha256.Sum256(fmp4)
	hash := fmt.Sprintf("%x", sum)
	rec := checkpoint.SegmentRecord{
		ID: seg.ID, File: fileName, SHA256: hash, Size: int64(len(fmp4)),
		StartNS: seg.StartNS, EndNS: seg.EndNS, DurationNS: seg.DurationNS,
		AUIndices: append([]int(nil), seg.AUIndices...),
	}
	if err := rn.journal.AddSegment(rec); err != nil {
		return err
	}
	if err := rn.journal.SaveAtomic(rn.journalPath); err != nil {
		return err
	}
	rn.manifestSegments = append(rn.manifestSegments, ManifestSegment{
		ID: seg.ID, File: fileName, SHA256: hash, Size: int64(len(fmp4)),
		StartNS: seg.StartNS, EndNS: seg.EndNS, DurationNS: seg.DurationNS,
		AUStart: seg.KeyframeIndex, AUEndExclusive: auEnd, AlignErrorNS: align,
	})
	if rn.opts.InterruptAfter > 0 && len(rn.journal.Segments) >= rn.opts.InterruptAfter {
		return ErrInterrupted
	}
	return nil
}
