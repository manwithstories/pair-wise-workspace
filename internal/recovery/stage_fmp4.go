package recovery

import (
	"mediad/internal/clock"
	"mediad/internal/fault"
	"mediad/internal/manifest"
)

func (e *exec) stageFMP4() error {
	for _, seg := range e.segs {
		if seg.hls.InitURI == "" {
			return fault.New(fault.InitMissing, e.hls.Path, seg.hls.LineOffset,
				"segment %d in period %d has no EXT-X-MAP after discontinuity",
				seg.index, seg.hls.Period)
		}
		initPath := manifest.ResolveURI(e.hls.Path, seg.hls.InitURI)
		initData, initHash, err := readAndHash(initPath)
		if err != nil {
			return err
		}
		if _, err := manifest.ValidateInit(initPath, initData); err != nil {
			return err
		}

		fragment, err := manifest.ParseFragment(seg.path, seg.data)
		if err != nil {
			return err
		}
		seg.fragment = fragment

		var mdatBoxes []manifest.Box
		for _, b := range fragment.Boxes {
			if b.Type == "mdat" {
				mdatBoxes = append(mdatBoxes, b)
			}
		}
		if len(mdatBoxes) == 0 {
			return fault.New(fault.InvalidManifest, seg.path, int64(seg.index), "fragment has no mdat box")
		}
		if err := manifest.CheckAVCCLengths(seg.path, seg.data, mdatBoxes[0]); err != nil {
			return err
		}

		stamp := clock.Stamp{
			SegmentIndex: seg.index,
			Period:       seg.hls.Period,
			PTS:          seg.hls.Start,
			DTS:          seg.hls.Start,
			Duration:     seg.hls.Duration,
		}
		if err := e.tl.Observe(seg.path, stamp, seg.hls.InitURI); err != nil {
			return err
		}

		state, ok := e.cp.Segment(seg.hash)
		if !ok {
			state = &SegmentState{Index: seg.index, SourceSHA256: seg.hash, ArtifactHashes: map[string]string{}}
		}
		state.Period = seg.hls.Period
		state.InitSHA256 = initHash
		if state.ArtifactHashes == nil {
			state.ArtifactHashes = map[string]string{}
		}
		if stageIndex(state.CompletedStage) < stageIndex("fmp4") {
			state.CompletedStage = "fmp4"
		}
		e.cp.PutSegment(state)
	}
	e.sum.Periods = len(e.tl.Periods)
	return e.finishStage("fmp4")
}
