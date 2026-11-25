package pipeline

import (
	"fmt"
	"math"

	"mediad/internal/checkpoint"
	"mediad/internal/codec"
	"mediad/internal/cut"
	"mediad/internal/dedup"
	"mediad/internal/pack"
	"mediad/internal/sample"
	"mediad/internal/silence"
	"mediad/internal/timeline"
)

func buildInit() ([]byte, error) { return pack.BuildInit() }

func (rn *runner) loadContent(ref dedup.Ref) (dedup.Content, error) {
	c, ok := rn.contents[ref.Location]
	if !ok {
		return dedup.Content{}, timeline.New(timeline.KindInternal, "pipeline.dedup_load", -1, -1,
			"missing prior content %s", ref.Location)
	}
	return c, nil
}

func (rn *runner) processAU(i int) error {
	idx := rn.stream.Indexes[i]
	rep := rn.reports[i]
	c := rn.opts.Codec

	if !rep.Valid {
		if rn.firstErrorByte < 0 || (rep.FirstErrorByte >= 0 && rep.FirstErrorByte < rn.firstErrorByte) {
			rn.firstErrorByte = rep.FirstErrorByte
		}
	} else {
		end := idx.Offset + int64(idx.Length)
		au := rn.stream.Blob[idx.Offset:end]
		if err := rn.mem.acquire(int64(len(au))); err != nil {
			return err
		}
		defer rn.mem.release(int64(len(au)))
		rn.actualBytes += int64(idx.Length)

		injectRTPDup := rn.opts.InjectDuplicate && i == 5
		if err := rn.flow.sendAU(uint32(idx.PTS33), au, rn.spans[i].StartNS, injectRTPDup); err != nil {
			return err
		}

		fp := dedup.Compute(dedup.Content{Codec: c, Config: rn.stream.Config, AU: au})
		loc := fmt.Sprintf("%s/node-a/au-%d", c, i)
		content := dedup.Content{Codec: c, Config: rn.stream.Config, AU: au}
		rn.contents[loc] = content
		if _, _, err := rn.store.CheckAndAdd(fp, content, loc, rn.loadContent); err != nil {
			return err
		}

		// The same AU arriving from a second regional node is a cross-node
		// duplicate. Byte verification happens inside CheckAndAdd.
		if i == sample.DuplicateAU && !rn.crossNodeChecked {
			rn.crossNodeChecked = true
			crossLoc := fmt.Sprintf("%s/node-b/au-%d", c, i)
			rn.contents[crossLoc] = content
			if _, _, err := rn.store.CheckAndAdd(fp, content, crossLoc, rn.loadContent); err != nil {
				return err
			}
		}
	}

	segs, err := rn.cutter.Add(cut.AU{
		Index: i, StartNS: rn.spans[i].StartNS, DurationNS: idx.DurationNS,
		Keyframe: idx.Keyframe, Valid: rep.Valid,
	})
	if err != nil {
		return err
	}
	for _, seg := range segs {
		if err := rn.commitSegment(seg, i); err != nil {
			return err
		}
	}
	return nil
}

func (rn *runner) buildManifest(sil silence.Result) *Manifest {
	total := len(rn.stream.Indexes)
	complete := codec.ValidAUCount(rn.reports)
	bad := total - complete
	durationNS := sample.DurationNS

	var coveredNS int64
	for _, s := range rn.manifestSegments {
		coveredNS += s.DurationNS
	}
	keyCoverage := 100 * float64(coveredNS) / float64(durationNS)
	if keyCoverage > 100 {
		keyCoverage = 100
	}
	actualBPS := rn.actualBytes * 8 * timeline.NSPerSecond / durationNS
	target := rn.opts.Media.TargetBPS
	deltaPct := 100 * (float64(actualBPS) - float64(target)) / float64(target)

	segments := rn.manifestSegments
	if segments == nil {
		segments = []ManifestSegment{}
	}
	silenceIntervals := sil.Silence
	if silenceIntervals == nil {
		silenceIntervals = []silence.Span{}
	}
	unknownIntervals := sil.Unknown
	if unknownIntervals == nil {
		unknownIntervals = []silence.Span{}
	}
	return &Manifest{
		Version: checkpoint.Version,
		Codec:   string(rn.opts.Codec), Seed: rn.opts.Media.Seed,
		TargetBPS: target, ActualBPS: actualBPS, DeltaPct: math.Round(deltaPct*1000) / 1000,
		DriftPPM: rn.opts.Media.DriftPPM, TimescaleNS: pack.TimescaleNS,
		InitFile: rn.initName, Segments: segments,
		TotalAUs: total, CompleteAUs: complete, BadAUs: bad,
		CompleteAUPct:    math.Round(10000*float64(complete)/float64(total)) / 100,
		KeyframeCoverage: math.Round(keyCoverage*100) / 100,
		SilenceNS:        sil.SilenceNS, SilenceIntervals: silenceIntervals,
		UnknownNS: sil.UnknownNS, UnknownIntervals: unknownIntervals,
		DuplicateAUs: rn.store.DuplicateAUs, DupSavedBytes: rn.store.SavedBytes,
		AlignMaxAbsNS: rn.alignMax, AlignMaxMS: float64(rn.alignMax) / 1e6,
		RTPDuplicates: rn.flow.RTPDuplicates, PausedReads: rn.pausedReads,
		FirstErrorByte: rn.firstErrorByte,
	}
}
