package pipeline

import (
	"os"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
	"mediascan/internal/stream"
	"mediascan/internal/subtitle"
)

// scanSubtitle parses an SRT/VTT file and runs its events through the
// timecode clock. Parser faults keep their raw byte offsets; clock faults
// (for example an unconvertible wrap) are reported, never zeroed silently.
func scanSubtitle(in Input, params Params) ([]fault.Fault, int, error) {
	f, err := os.Open(in.Path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var res *subtitle.Result
	if in.Kind == KindSRT {
		res, err = subtitle.ParseSRT(f, in.Path)
	} else {
		res, err = subtitle.ParseVTT(f, in.Path)
	}
	if err != nil {
		return nil, 0, err
	}
	flts := append([]fault.Fault(nil), res.Faults...)

	// Timeline transform: map each event onto the frame-rate clock, resolving
	// cross-midnight wraps relative to the previous event.
	var prevFrames int64 = -1
	usable := 0
	for _, ev := range res.Events {
		tc, perr := clock.FromFrames(ev.StartMS*int64(params.Rate.Num)/1000, params.Rate)
		if perr != nil {
			flts = append(flts, fault.New(in.Path, ev.RawStart, fault.StageTimecode, true,
				"event at offset %d cannot be mapped to clock: %v", ev.RawStart, perr))
			continue
		}
		frames, ferr := clock.ToFrames(tc, params.Rate)
		if ferr != nil {
			flts = append(flts, fault.New(in.Path, ev.RawStart, fault.StageTimecode, true,
				"unconvertible timecode at offset %d: %v", ev.RawStart, ferr))
			continue
		}
		if prevFrames >= 0 {
			if _, werr := clock.ResolveWrap(tc, params.Rate, prevFrames); werr != nil {
				flts = append(flts, fault.New(in.Path, ev.RawStart, fault.StageTimecode, true,
					"wrap resolution failed at offset %d: %v", ev.RawStart, werr))
				continue
			}
		}
		prevFrames = frames
		usable++
	}
	return flts, usable, nil
}

// scanMP4 hands structural and keyframe analysis to the stream package and
// folds its byte-anchored faults into the unified fault list.
func scanMP4(in Input, seg SegmentReport) ([]fault.Fault, SegmentReport, error) {
	f, err := os.Open(in.Path)
	if err != nil {
		return nil, seg, err
	}
	defer f.Close()
	st, err := stream.AnalyzeMP4(f, in.Path)
	if err != nil {
		return nil, seg, err
	}
	seg.BoxCount = len(st.Boxes)
	seg.KeyframeGaps = len(st.KeyframeGaps)
	seg.DamageOffset = st.DamageOffset
	if st.RebuiltIndex {
		// Index rebuilt from readable regions: the segment stays usable.
		seg.Recovered = true
	}
	return append([]fault.Fault(nil), st.Faults...), seg, nil
}

// scanRTP folds RTP analysis into the segment report. Network gaps remain
// offset-free (offset -1); only file-level damage carries byte positions.
func scanRTP(in Input, seg SegmentReport) ([]fault.Fault, SegmentReport, error) {
	f, err := os.Open(in.Path)
	if err != nil {
		return nil, seg, err
	}
	defer f.Close()
	st, err := stream.AnalyzeRTP(f, in.Path)
	if err != nil {
		return nil, seg, err
	}
	seg.PacketCount = st.Packets
	seg.RTPGaps = len(st.Gaps)
	if st.OutOfWindow > 0 || len(st.Gaps) > 0 {
		seg.Recovered = true
	}
	return append([]fault.Fault(nil), st.Faults...), seg, nil
}
