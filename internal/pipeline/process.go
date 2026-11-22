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
		faults = sample.BoundaryFaults(c.Seed+int64(spec.rep.Bitrate), count, c.LateWindowUS, spec.opts.PacketUS, c.SegmentUS)
	}
	// Only the AAC representation loses its tail. Its 21333us packets do not
	// divide the 120s programme, so the final packets are the only audio past
	// the shared end; their loss leaves an unbounded gap that must be declared
	// unrecoverable rather than guessed.
	if spec.rep.Codec == model.CodecAAC {
		faults.TailDrop = 2
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
	r.accountGap(st, ev)
	if r.wm.Late(ev.ExtTimeUS, ev.ExtTimeUS+ev.DurationUS) {
		// Beyond the configured lateness window: isolate with the reason and
		// leave already committed segments untouched.
		r.quarantine = append(r.quarantine, quarantineOf(ev, model.KindLate,
			fmt.Sprintf("arrived at %d us, watermark is %d us", ev.ArrivalUS, r.wm.Value())))
		st.quarant[ev.ID.Seq] = model.KindLate
		r.observe(ev)
		return nil
	}
	// Capture which segments were already committed before folding this event
	// in: those are the ones a late arrival has to recalculate.
	touched := r.committedTouching(st, ev)
	st.insert(ev)
	r.observe(ev)
	for _, idx := range touched {
		r.commitSegment(st, idx, true, ev.ID, ev.ExtTimeUS)
	}
	return nil
}

// committedTouching lists committed segment indices the event overlaps. A
// segment older than the replay horizon can no longer be rebuilt because its
// events have been pruned; such an arrival is quarantined instead.
func (r *Runner) committedTouching(st *streamState, ev *model.Event) []int {
	end := ev.ExtTimeUS + max64(ev.DurationUS, 0)
	var out []int
	for _, idx := range timeline.AffectedSegments(ev.ExtTimeUS, end, r.cfg.SegmentUS) {
		if idx >= st.frontier {
			continue
		}
		if idx < st.frontier-2 {
			r.quarantine = append(r.quarantine, quarantineOf(ev, model.KindLate,
				fmt.Sprintf("segment %d is beyond the replay horizon %d", idx, st.frontier-2)))
			st.quarant[ev.ID.Seq] = model.KindLate
			continue
		}
		out = append(out, idx)
	}
	return out
}

// accountGap settles the hole ending just before ev, exactly once per hole.
// Because gapCursor only moves forward, a reordered or replayed packet can
// never re-open a hole that was already accounted. The decision uses the same
// recovery policy as the segment builder, so reporting and manifest agree.
func (r *Runner) accountGap(st *streamState, ev *model.Event) {
	if !st.hasGapCursor {
		st.gapCursor = int64(ev.ID.Seq)
		st.gapEndUS = ev.ExtTimeUS + max64(ev.DurationUS, 0)
		st.hasGapCursor = true
		return
	}
	if int64(ev.ID.Seq) <= st.gapCursor {
		return // retransmission or reordering behind the cursor: already settled
	}
	missing := int64(ev.ID.Seq) - st.gapCursor - 1
	if missing <= 0 {
		st.gapCursor = int64(ev.ID.Seq)
		st.gapEndUS = ev.ExtTimeUS + max64(ev.DurationUS, 0)
		return
	}
	corruptOnly := true
	for seq := st.gapCursor + 1; seq < int64(ev.ID.Seq); seq++ {
		if kind, ok := st.quarant[uint32(seq)]; !ok || kind != model.KindCorrupt {
			corruptOnly = false
			break
		}
	}
	reason := model.KindUnknownGap
	if corruptOnly {
		reason = model.KindCorrupt
	}
	d := recovery.Decide(recovery.Evidence{
		HoleSeq: uint32(st.gapCursor + 1), HoleStartUS: st.gapEndUS,
		PreviousEndUS: st.gapEndUS, NextStartUS: ev.ExtTimeUS,
		HasNext: true, MaxFillUS: r.cfg.MaxFillUS, Reason: reason,
	})
	if corruptOnly {
		r.recoveryStats.CorruptHoles++
	} else {
		r.recoveryStats.LossHoles++
	}
	switch {
	case d.Recoverable && corruptOnly:
		r.recoveryStats.RecoveredCorrupt++
		r.recoveryStats.EstimatedUS += d.DurationUS
	case d.Recoverable:
		r.recoveryStats.RecoveredLoss++
		r.recoveryStats.EstimatedUS += d.DurationUS
	default:
		r.recoveryStats.Unrecoverable++
	}
	st.gapCursor = int64(ev.ID.Seq)
	st.gapEndUS = ev.ExtTimeUS + max64(ev.DurationUS, 0)
}

// settleTail declares an unbounded end gap when the programme's audio stops
// before its nominal duration. It runs once, at the end of a completed run.
func (r *Runner) settleTail() {
	for _, st := range r.streams {
		if !st.hasEvent {
			continue
		}
		if st.coverEnd >= r.cfg.DurationUS {
			continue
		}
		r.recoveryStats.LossHoles++
		r.recoveryStats.Unrecoverable++
	}
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
	// Idempotency: re-submitting a slot that is still inside the replay horizon
	// with byte-identical content is a no-op, and a slot outside the horizon
	// cannot be recalculated at all (its source events were pruned). Neither
	// case may extend the output chain, so a repeated submission can never
	// change the result hash.
	if index < st.frontier-2 {
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
	// Track the worst actual packet-edge deviation from the grid; pruning keeps
	// this bounded by the replay horizon.
	r.skewBySlot[k] = seg.BoundarySkew
	if seg.Index >= 2 {
		delete(r.skewBySlot, key(seg.Rep, seg.Index-2))
	}
	if r.materialize != nil && r.materializeErr == nil {
		// The digest describes the media content, so the sample payloads are
		// hashed into it as well as the metadata. Writing happens after the
		// digest is fixed; the file name already carries the version.
		built, err := r.materialize(st, ent)
		if err != nil {
			r.materializeErr = err
		} else {
			ent.MediaSize = built.MediaSize
			ent.InitSHA256 = built.InitSHA256
			ent.MediaSHA256 = built.MediaSHA256
			r.encodedBytes += built.MediaSize
		}
	}
	ent.OutputSHA256 = segmentOutputDigest(ent)
	if prev, ok := r.committed[k]; ok && prev.version == ent.Version && prev.digest == ent.OutputSHA256 {
		return // identical re-submission: nothing to append
	}
	r.committed[k] = committedSlot{version: ent.Version, digest: ent.OutputSHA256}
	if index >= 3 {
		delete(r.committed, key(st.spec.rep.ID, index-3))
	}
	seg.OutputSHA256 = ent.OutputSHA256
	if !r.counted[k] {
		r.counted[k] = true
		r.segCount++
	}
	r.latest[k] = seg
	r.latestSHA[k] = ent.OutputSHA256
	// Keep only the newest slots per representation so memory stays bounded by
	// the replay horizon, never by the archive length.
	if seg.Index >= 2 {
		old := key(seg.Rep, seg.Index-2)
		delete(r.latest, old)
		delete(r.latestSHA, old)
	}
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

// recordFrame records the two cross-representation measures for a segment:
//
//   - boundary delta: the difference between the output start instants of the
//     representations. All representations are cut on the common grid, so this
//     is the actual delivered alignment and must be zero.
//   - sync offset: the difference between the representations' real packet
//     edges at that boundary. Packets of different durations cannot land on
//     the same instant, so this quantifies how far the underlying audio is
//     offset even though the output boundaries are exact.
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
	var minStart, maxStart int64 = -1, -1
	var minErr, maxErr int64
	first := true
	for _, s := range m {
		// A window without audio, or one whose audio stops short of the
		// boundary (a truncated tail), has no meaningful sync sample: the
		// remainder is labelled silence, not a rendition offset.
		if s.FrameCount == 0 || s.SourceEnd < s.EndUS {
			delete(r.pending, seg.Index)
			return
		}
		if minStart < 0 || s.StartUS < minStart {
			minStart = s.StartUS
		}
		if maxStart < 0 || s.StartUS > maxStart {
			maxStart = s.StartUS
		}
		// Signed distance between the representation's real packet edge and the
		// nominal grid boundary. This is the residual sync offset a player would
		// hear between renditions even though the cuts themselves are aligned.
		err := s.SourceEnd - s.EndUS
		if first || err < minErr {
			minErr = err
		}
		if first || err > maxErr {
			maxErr = err
		}
		first = false
	}
	if d := maxStart - minStart; d > r.boundaryMax {
		r.boundaryMax = d
	}
	if first {
		// No representation carried audio in this window: alignment is
		// undefined rather than zero.
		delete(r.pending, seg.Index)
		return
	}
	// Observations are microseconds; the histogram reports whole milliseconds.
	r.syncHist.Observe(abs64(maxErr-minErr) / 1000)
	delete(r.pending, seg.Index)
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
		// Events are pruned to the replay horizon, and events far outside the
		// window arrive here during a replay; only overlapping ones count.
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
	decisions, _ := timeline.GapsTo(st.events, st.quarant, r.cfg.MaxFillUS, r.cfg.DurationUS)
	for _, d := range decisions {
		// Attribute each hole to the window containing its start, and only to a
		// window whose own events make the hole visible. After pruning, a hole
		// left behind by earlier packets must not be attributed again.
		if d.StartUS < startUS || d.StartUS >= endUS {
			continue
		}
		if d.StartUS < st.events[0].ExtTimeUS {
			continue
		}
		if !d.Recoverable {
			ent.UnrecoverableGaps++
			ent.RecoveryOps = appendUnique(ent.RecoveryOps, string(recovery.ActionUnrecoverable))
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

// committedSlot is the digest of the newest committed version of a slot. It
// backs idempotent duplicate submission and is bounded to the replay horizon.
type committedSlot struct {
	version int
	digest  string
}

// Materialized holds the on-disk artefacts of one segment version.
type Materialized struct {
	InitSHA256  string
	MediaSHA256 string
	MediaSize   int64
}
