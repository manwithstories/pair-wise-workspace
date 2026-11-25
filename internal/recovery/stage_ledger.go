package recovery

import (
	"sort"

	"mediad/internal/clock"
	"mediad/internal/codec"
)

func (e *exec) stageLedger() error {
	ledger, err := LoadLedger(e.opts.Root)
	if err != nil {
		return err
	}
	versions := VersionInfo{
		SpecVersion:    codec.SupportedSpecVersion,
		AdapterVersion: codec.AdapterVersion,
		Pipeline:       "mediad-pipeline-v1",
		GoMP4:          "v1.4.1",
		PionRTP:        "v1.10.6",
	}

	frame := 0
	for _, seg := range e.segs {
		state, ok := e.cp.Segment(seg.hash)
		if !ok {
			state = &SegmentState{ArtifactHashes: map[string]string{}}
		}
		end, err := clock.Add(seg.hls.Start, seg.hls.Duration)
		if err != nil {
			return err
		}
		timeRange := clock.TimeRange{
			Track: "av", Period: seg.hls.Period,
			Start: seg.hls.Start.String(), End: end.String(),
			Seconds: seg.hls.Duration.Float64(),
		}

		var keyframes []int
		for local, n := range seg.nals {
			if n.IDR {
				keyframes = append(keyframes, frame+local)
			}
		}
		lastMap := e.maps[frame+len(seg.nals)-1]
		artifacts := map[string]string{}
		for k, v := range state.ArtifactHashes {
			artifacts[k] = v
		}
		if h := e.cp.Artifacts["master_wav"]; h != "" {
			artifacts["master_wav"] = h
		}

		record := Record{
			RecordID:        seg.hash,
			SegmentIndex:    seg.index,
			Period:          seg.hls.Period,
			SourceURI:       seg.hls.URI,
			SourceSHA256:    seg.hash,
			InitURI:         seg.hls.InitURI,
			InitSHA256:      state.InitSHA256,
			Params:          e.params,
			Versions:        versions,
			OutputSHA256:    state.ArtifactHashes["pcm16"],
			OutputArtifacts: artifacts,
			TimeRanges:      []clock.TimeRange{timeRange},
			Keyframes:       keyframes,
			KeyframeCount:   len(keyframes),
			RTPExtSequence:  lastMap.ExtSequence,
			RTPExtTimestamp: lastMap.ExtTimestamp,
		}
		if _, err := ledger.Append(record); err != nil {
			return err
		}
		frame += len(seg.nals)
	}

	sort.Slice(ledger.Records, func(i, j int) bool {
		return ledger.Records[i].SegmentIndex < ledger.Records[j].SegmentIndex
	})
	if err := ledger.WriteRewritten(); err != nil {
		return err
	}
	return e.finishStage("ledger")
}
