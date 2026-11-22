package pipeline

import (
	"fmt"
	"time"

	"example.com/medialab/archive/internal/ingest"
	"example.com/medialab/archive/internal/model"
	"example.com/medialab/archive/internal/provenance"
	"example.com/medialab/archive/internal/recovery"
	"example.com/medialab/archive/internal/sample"
	"example.com/medialab/archive/internal/segment"
	"example.com/medialab/archive/internal/timeline"
)

// buildPlan generates the sample plan for one stream from the damage profile.
func buildPlan(c Config, spec *streamSpec) (*sample.Plan, error) {
	count := sample.PacketCount(c.DurationUS, spec.opts.PacketUS)
	var faults sample.Faults
	switch c.Damage {
	case DamageBurstLoss:
		faults = sample.BurstLossFaults(c.Seed+int64(spec.rep.Bitrate), count)
	case DamageMixed:
		faults = sample.MixedFaults(c.Seed+int64(spec.rep.Bitrate), count)
	case DamageNone:
		faults = sample.NoFaults()
	default:
		return nil, fmt.Errorf("pipeline: unknown damage %q", c.Damage)
	}
	if c.LateAtBoundary {
		faults = sample.BoundaryFaults(c.Seed+int64(spec.rep.Bitrate), count, c.LateWindowUS, spec.opts.PacketUS)
	}
	return sample.GenerateWithFaults(spec.opts, faults)
}

// sampleRecord adapts a log entry to the ingest record type.
func sampleRecord(e logEntry) ingest.RawRecord {
	return ingest.RawRecord{ArrivalUS: e.ArrivalUS, Bytes: e.Bytes}
}

// process folds one arriving record into the timeline.
func (r *Runner) process(entry logEntry) error {
	st := r.streams[entry.Stream]
	ev, err := st.parser.Parse(sampleRecord(entry))
	if ev == nil {
		return nil
	}
	if ev.PayloadSHA == "" {
		ev.PayloadSHA = ev.PayloadHash()
	}
	// Idempotent duplicate detection: the same stream+sequence+payload is
	// counted once and never adds audio time twice.
	if prev, ok := st.dedupe[ev.ID.Seq]; ok {
		if prev == ev.PayloadSHA {
			st.parser.MarkDuplicate()
			r.dupes++
			return nil
		}
		r.conflicts++
		r.quarantine = append(r.quarantine, quarantineOf(ev, model.KindInvalidPacket,
			"sequence reused with a different payload"))
		st.quarant[ev.ID.Seq] = model.KindInvalidPacket
		return nil
	}
	st.dedupe[ev.ID.Seq] = ev.PayloadSHA
	st.accepted++
	if ev.DurationUS > 0 {
		st.audioUS += ev.DurationUS
		st.hasEvent = true
		if end := ev.ExtTimeUS + ev.DurationUS; end > st.coverEnd {
			st.coverEnd = end
		}
	}
	if err != nil {
		// The payload could not prove its timing: isolate it, never guess.
		// Corrupt events still count towards coverage only by their arrival
		// evidence, never by a fabricated duration.
		kind := model.KindOf(err)
		r.quarantine = append(r.quarantine, quarantineOf(ev, kind, err.Error()))
		st.quarant[ev.ID.Seq] = kind
		return nil
	}
	st.maxSeq = max64(st.maxSeq, int64(ev.ID.Seq))
	if r.wm.Late(ev.ExtTimeUS, ev.ExtTimeUS+ev.DurationUS) {
		// Past the watermark: isolate with the reason, do not touch commits.
		r.quarantine = append(r.quarantine, quarantineOf(ev, model.KindLate,
			fmt.Sprintf("arrived at %d us, watermark is %d us", ev.ArrivalUS, r.wm.Value())))
		st.quarant[ev.ID.Seq] = model.KindLate
		r.observe(ev)
		return nil
	}
	st.insert(ev)
	r.observe(ev)
	// In-window late events force the affected committed segments to replay.
	r.replayAffected(st, ev)
	return nil
}

// observe advances the watermark and commits newly covered segments.
func (r *Runner) observe(ev *model.Event) {
	r.wm.Observe(ev.ExtTimeUS + max64(ev.DurationUS, 0))
	for _, st := range r.streams {
		r.commitCovered(st)
	}
}

// insert keeps the active window in canonical order.
func (st *streamState) insert(ev *model.Event) {
	st.events = append(st.events, ev)
	for i := len(st.events) - 1; i > 0; i-- {
		if eventLess(st.events[i-1], st.events[i]) {
			break
		}
		st.events[i-1], st.events[i] = st.events[i], st.events[i-1]
	}
}

func eventLess(a, b *model.Event) bool {
	if a.ExtTimeUS != b.ExtTimeUS {
		return a.ExtTimeUS < b.ExtTimeUS
	}
	if a.ID.Stream != b.ID.Stream {
		return a.ID.Stream < b.ID.Stream
	}
	if a.ID.Seq != b.ID.Seq {
		return a.ID.Seq < b.ID.Seq
	}
	return a.PayloadSHA < b.PayloadSHA
}

// replayAffected recalculates any committed segment the event touches. It is
// deterministic: the same event and window produce the same version.
func (r *Runner) replayAffected(st *streamState, ev *model.Event) {
	end := ev.ExtTimeUS + max64(ev.DurationUS, 0)
	for _, idx := range timeline.AffectedSegments(ev.ExtTimeUS, end, r.cfg.SegmentUS) {
		if idx < st.frontier {
			r.commitSegment(st, idx, true, ev.ID, ev.ExtTimeUS)
		}
	}
}

// prune drops events that no open segment can still need, bounding memory by
// the late window and the current segment rather than by archive length.
func (r *Runner) prune(st *streamState, keepFromUS int64) {
	cut := 0
	for cut < len(st.events) && st.events[cut].ExtTimeUS+max64(st.events[cut].DurationUS, 0) <= keepFromUS {
		cut++
	}
	if cut > 0 {
		st.events = append([]*model.Event(nil), st.events[cut:]...)
	}
	// Duplicate and quarantine bookkeeping only needs to span the late window:
	// a repeat or a late copy arrives within it by definition.
	windowPackets := (r.cfg.LateWindowUS+2*r.cfg.SegmentUS)/st.plan.Opts.PacketUS + 64
	keepSeq := int64(0)
	if st.maxSeq > windowPackets {
		keepSeq = st.maxSeq - windowPackets
	}
	for seq := range st.dedupe {
		if int64(seq) < keepSeq && len(st.dedupe) > int(windowPackets) {
			delete(st.dedupe, seq)
		}
	}
	for seq := range st.quarant {
		if int64(seq) < keepSeq && len(st.quarant) > int(windowPackets) {
			delete(st.quarant, seq)
		}
	}
}

// commitSegment builds and submits one segment version.
func (r *Runner) commitSegment(st *streamState, index int, replay bool, cause model.PacketID, causeUS int64) {
	startUS, endUS := segStart(index, r.cfg.SegmentUS), segEnd(index, r.cfg.SegmentUS)
	if endUS > r.cfg.DurationUS {
		endUS = r.cfg.DurationUS
	}
	if endUS <= startUS {
		return
	}
	begin := time.Now()
	seg, ent := r.buildSegment(st, index, startUS, endUS)
	k := key(st.spec.rep.ID, index)
	if replay {
		rep := r.versions.Promote(st.spec.rep.ID, index, startUS, endUS, cause, causeUS)
		ent.Version = rep.ToVersion
		ent.ReplacesVersion = rep.FromVersion
		ent.ReplacesSHA256 = r.latestSHA[k]
		ent.Reason = rep.Reason
		ent.CauseSeq = cause.Seq
		ent.CauseTimeUS = causeUS
	} else {
		r.versions.Record(st.spec.rep.ID, index)
		ent.Version = r.versions.Current(timeline.SegmentKey{Rep: st.spec.rep.ID, Index: index})
	}
	seg.Version = ent.Version
	seg.LossFills, seg.CorruptFills, seg.UnrecoverableGaps = ent.LossFills, ent.CorruptFills, ent.UnrecoverableGaps
	seg.SilenceUS = ent.SilenceUS
	r.applyRecovery(k, seg, replay)
	if r.materialize != nil {
		built, err := r.materialize(st, ent)
		if err != nil {
			r.materializeErr = err
		} else {
			ent.MediaSize = built.MediaSize
			ent.InitSHA256 = built.InitSHA256
			r.encodedBytes += built.MediaSize
		}
	}
	ent.OutputSHA256 = segmentOutputDigest(ent)
	seg.OutputSHA256 = ent.OutputSHA256
	r.latest[k] = seg
	r.latestSHA[k] = ent.OutputSHA256
	r.chainSegment(ent)
	r.recordFrame(seg)
	r.elapsed(begin)
	if r.manifest != nil {
		if _, err := r.manifest.Commit(*ent); err != nil {
			r.conflicts++
		}
	}
	if r.ledger != nil {
		le := entryForLedger(*ent, seg, st)
		if _, err := r.ledger.Record(le); err != nil {
			r.conflicts++
		}
	}
}

// applyRecovery updates the running hole totals by replacing the previous
// contribution of this slot, so a replay corrects rather than double counts.
// The per-slot row is dropped once the slot can no longer be replayed, which
// keeps memory bounded by the active window.
func (r *Runner) applyRecovery(slot string, seg model.Segment, replay bool) {
	if replay {
		if prev, ok := r.slotFill[slot]; ok {
			r.recoveryStats.LossHoles -= prev.loss
			r.recoveryStats.RecoveredLoss -= prev.loss
			r.recoveryStats.CorruptHoles -= prev.corrupt
			r.recoveryStats.RecoveredCorrupt -= prev.corrupt
			r.recoveryStats.Unrecoverable -= prev.unrec
			r.recoveryStats.EstimatedUS -= prev.silence
		}
	}
	row := fillRow{loss: int64(seg.LossFills), corrupt: int64(seg.CorruptFills),
		unrec: int64(seg.UnrecoverableGaps), silence: seg.SilenceUS}
	r.slotFill[slot] = row
	r.recoveryStats.LossHoles += row.loss
	r.recoveryStats.RecoveredLoss += row.loss
	r.recoveryStats.CorruptHoles += row.corrupt
	r.recoveryStats.RecoveredCorrupt += row.corrupt
	r.recoveryStats.Unrecoverable += row.unrec
	r.recoveryStats.EstimatedUS += row.silence
	if seg.Index >= 2 {
		delete(r.slotFill, key(seg.Rep, seg.Index-2))
	}
}

type fillRow struct {
	loss    int64
	corrupt int64
	unrec   int64
	silence int64
}

// Materialized holds the on-disk artefacts of one segment version.
type Materialized struct {
	InitSHA256 string
	MediaSize  int64
}

// recordFrame records the cross-representation sync metric for a segment.
func (r *Runner) recordFrame(seg model.Segment) {
	m := r.pending[seg.Index]
	if m == nil {
		m = map[model.RepresentationID]model.Segment{}
		r.pending[seg.Index] = m
	}
	m[seg.Rep] = seg
	if len(m) < len(r.streams) {
		return
	}
	// All representations have committed this index: compare boundaries.
	var minStart, maxStart int64 = -1, -1
	for _, s := range m {
		if minStart < 0 || s.StartUS < minStart {
			minStart = s.StartUS
		}
		if maxStart < 0 || s.StartUS > maxStart {
			maxStart = s.StartUS
		}
	}
	r.syncHist.Observe(maxStart - minStart)
	if seg.Index < 2 {
		delete(r.pending, seg.Index)
	}
}

// buildSegment assembles the model segment and its manifest entry.
func (r *Runner) buildSegment(st *streamState, index int, startUS, endUS int64) (model.Segment, *segment.Entry) {
	seg := model.Segment{Rep: st.spec.rep.ID, Index: index, StartUS: startUS, EndUS: endUS}
	ent := &segment.Entry{
		Rep: st.spec.rep.ID, Index: index, StartUS: startUS, EndUS: endUS,
		SourceStream: st.spec.cfg.ID,
	}
	var minSeq, maxSeq uint32
	count := 0
	for _, ev := range st.events {
		evEnd := ev.ExtTimeUS + max64(ev.DurationUS, 0)
		if ev.DurationUS <= 0 {
			if ev.ExtTimeUS >= startUS && ev.ExtTimeUS < endUS {
				ent.QuarantinedCount++
			}
			continue
		}
		if evEnd <= startUS || ev.ExtTimeUS >= endUS {
			continue
		}
		if count == 0 || ev.ID.Seq < minSeq {
			minSeq = ev.ID.Seq
		}
		if count == 0 || ev.ID.Seq > maxSeq {
			maxSeq = ev.ID.Seq
		}
		count++
		if count == 1 || ev.ExtTimeUS < seg.SourceStart {
			seg.SourceStart = ev.ExtTimeUS
		}
		if evEnd > seg.SourceEnd {
			seg.SourceEnd = evEnd
		}
	}
	seg.FrameCount = count
	ent.FrameCount = count
	ent.SourceSeqMin = minSeq
	ent.SourceSeqMax = maxSeq
	ent.SourcePacketCount = count
	ent.SourceStartUS = seg.SourceStart
	ent.SourceEndUS = seg.SourceEnd
	if count > 0 {
		seg.BoundarySkew = max64(abs64(seg.SourceStart-startUS), abs64(seg.SourceEnd-endUS))
		ent.BoundarySkewUS = seg.BoundarySkew
	}
	// Silence fill: attribute recoverable holes to the window that starts them
	// so each hole is counted exactly once across all segments and replays.
	decisions, _ := timeline.Gaps(st.events, st.quarant, r.cfg.MaxFillUS)
	for _, d := range decisions {
		if d.StartUS < startUS || d.StartUS >= endUS {
			continue
		}
		if !d.Recoverable {
			ent.UnrecoverableGaps++
			continue
		}
		ent.SilenceUS += d.DurationUS
		ent.RecoveredPackets++
		ent.RecoveryOps = appendUnique(ent.RecoveryOps, string(recovery.ActionSilenceFill))
		if d.Reason == model.KindCorrupt {
			ent.CorruptFills++
		} else {
			ent.LossFills++
		}
	}
	return seg, ent
}

// quarantineOf builds the shared quarantine record for one event.
func quarantineOf(ev *model.Event, kind model.ErrorKind, detail string) model.Quarantine {
	if kind == model.KindNone {
		kind = model.KindInvalidPacket
	}
	return model.Quarantine{
		ID: ev.ID, Kind: kind, Detail: detail, RTPTime: ev.RTPTime,
		ArrivalUS: ev.ArrivalUS, Seq: ev.ID.Seq, Stream: ev.ID.Stream,
		AtUS: ev.ExtTimeUS, PayloadSHA: ev.PayloadSHA,
	}
}

// entryForLedger converts a manifest entry to a provenance entry.
func entryForLedger(e segment.Entry, seg model.Segment, st *streamState) provenance.Entry {
	le := provenance.Entry{
		Rep: e.Rep, SegmentIndex: e.Index, Version: e.Version,
		StartUS: e.StartUS, EndUS: e.EndUS, SourceStream: e.SourceStream,
		SourceSeqMin: e.SourceSeqMin, SourceSeqMax: e.SourceSeqMax,
		SourcePacketCount: e.SourcePacketCount,
		RecoveryOps:       e.RecoveryOps, EstimatedUS: e.SilenceUS,
		TimelineVersion: e.Version, OutputSHA256: e.OutputSHA256,
		ReplacesVersion: e.ReplacesVersion, ReplacesSHA256: e.ReplacesSHA256,
	}
	for _, ev := range st.events {
		if ev.ExtTimeUS >= e.StartUS && ev.ExtTimeUS < e.EndUS {
			le.SourceTimes = append(le.SourceTimes, ev.ExtTimeUS)
		}
	}
	_ = seg
	return le
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
