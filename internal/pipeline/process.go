// Package pipeline per-track processing: fragments are parsed, hashed and
// measured in topological order before post-processing finalises the report.
package pipeline

import (
	"pairwise/ingestcheck/internal/audit"
	"pairwise/ingestcheck/internal/clock"
	"pairwise/ingestcheck/internal/mp4"
	"pairwise/ingestcheck/internal/recovery"
	rtpadapt "pairwise/ingestcheck/internal/rtp"
)

// processTrack runs one track's fragments in topological position, updating
// the shared report and the meter. It isolates corrupt fragments and keeps
// going so a single bad input cannot abort the run.
func (r *Runner) processTrack(id uint32, g Group, inputs []FragmentInput, outputs map[uint32]*trackOut, rep *Report, hasher *audit.Hasher, meter *recovery.Meter) error {
	out := &trackOut{report: TrackReport{TrackID: id}}
	w, werr := rtpadapt.New(r.cfg.RTP)
	if werr != nil {
		return werr
	}
	rate := trackRate(g.Graph, id)
	state := &trackState{timeline: clock.NewTimeline(rate)}

	for _, in := range inputs {
		boxes, perr := parseOne(in.Data, r.cfg.MP4)
		if perr != nil {
			r.isolate(out, rep, in, perr)
			r.feedPackets(w, in)
			_ = w.Flush()
			continue
		}
		rep.RoundTripTotal++
		if ok, rerr := roundTrip(boxes, r.cfg.MP4); rerr == nil && ok {
			rep.RoundTripOK++
		} else if rerr != nil {
			rep.Errors = append(rep.Errors, rerr.Error())
		}
		frag, ferr := mp4.ParseFragment(mp4.Find(boxes, "moof"))
		if ferr != nil {
			r.isolate(out, rep, in, ferr)
			r.feedPackets(w, in)
			_ = w.Flush()
			continue
		}
		out.report.Parsed++

		// Hash the fragment payload; identical inputs always hash equal.
		if herr := hasher.Add(in.TrackID, frag.Sequence, in.Data); herr != nil {
			return herr
		}
		rep.SegmentSHATotal++
		rep.SegmentSHAOK++

		// tfdt regression opens a new epoch without rewriting the tick.
		if state.haveBase && frag.BaseTime < state.prevBase {
			if e := state.timeline.Regress(state.wall, int64(frag.BaseTime)); e != nil {
				rep.Errors = append(rep.Errors, e.Error())
			}
			out.report.Epochs++
		}
		// An mfhd jump means whole fragments never arrived.
		if state.haveSeq && frag.Sequence > state.prevSeq+1 {
			missing := frag.Sequence - state.prevSeq - 1
			out.report.HeaderGaps += int(missing)
			outcome, _, _ := meter.Gap(int64(missing)*fragmentBytes(frag), int(missing))
			countRecovery(&out.report, rep, outcome)
		}
		// Packet loss within a fragment propagates the same way.
		if in.Missing > 0 {
			out.report.PacketGaps++
			outcome, _, _ := meter.Gap(int64(in.Missing)*fragmentBytes(frag), in.Missing)
			countRecovery(&out.report, rep, outcome)
		}
		if in.Bandwidth > 0 {
			meter.Observe(in.Bandwidth)
		}

		r.feedPackets(w, in)
		state.prevSeq = frag.Sequence
		state.haveSeq = true
		state.prevBase = frag.BaseTime
		state.haveBase = true
		state.wall += fragmentMicros(frag, rate)

		out.frags = append(out.frags, frag)
		out.starts = append(out.starts, mediaTick{tick: int64(frag.BaseTime), rate: rate})
	}
	_ = w.Flush()
	st := w.Stats()
	out.report.Duplicate = st.Duplicate
	out.report.Late = st.Late
	out.report.Lost = st.Lost
	out.report.MaxHeldPeak = st.MaxHeldPeak
	rep.DuplicateDrop += st.Duplicate
	rep.Tracks = append(rep.Tracks, out.report)
	outputs[id] = out

	return nil
}
