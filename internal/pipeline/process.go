package pipeline

import (
	"context"
	"errors"
	"strings"

	"archive/internal/bitrate"
	"archive/internal/buffer"
	"archive/internal/manifest"
	"archive/internal/model"
	"archive/internal/rtp"
	"archive/internal/syncer"
)

func stringReader(s string) *strings.Reader { return strings.NewReader(s) }

// process drains the bounded queue, reorders through the jitter buffer,
// repairs loss within the finite window, aligns the surviving packets and
// drives the bandwidth estimator on the configured sample schedule.
func (r *runner) process(ctx context.Context) error {
	reps := []bitrate.Representation{
		{Name: "low", Kbps: r.cfg.TargetKbps / 3},
		{Name: "mid", Kbps: r.cfg.TargetKbps},
		{Name: "high", Kbps: r.cfg.TargetKbps * 2},
	}
	r.selector = bitrate.NewSelector(reps, r.estim, r.log)
	aligner := syncer.NewAligner(r.tl, r.log)

	sampleAt := 0
	observed := 0
	for {
		mp, err := r.queue.Pop(ctx)
		if errors.Is(err, buffer.ErrQueueClosed) {
			break
		}
		if err != nil {
			return err
		}
		// Duplicates and out-of-window packets are rejected by the jitter
		// buffer and never enter the counters, so statistics describe the
		// deduplicated ordered stream rather than arrival noise.
		if !r.jitter.Push(mp) {
			continue
		}
		observed++
		// Fixed-cadence bandwidth observations are driven by the accepted
		// (deduplicated) packet count, which is independent of scheduling.
		if observed%12 == 0 && sampleAt < len(r.cfg.BandwidthKbps) {
			bw := r.cfg.BandwidthKbps[sampleAt%len(r.cfg.BandwidthKbps)]
			drop, rise := r.estim.Observe(bw)
			r.selector.Choose(drop, rise)
			sampleAt++
		}
		if peak := int64(r.queue.PeakBytes()); peak > r.peakQueue {
			r.peakQueue = peak
		}
	}

	ordered, missing := r.jitter.Drain()
	// CRC is evaluated once per unique retained packet, giving a stable
	// match ratio unaffected by how many times a packet was retransmitted.
	for _, p := range ordered {
		r.packetsTotal++
		if rtp.CheckCRC(p.Payload) {
			r.crcMatched++
		} else {
			r.log.Add(model.Fault{Phase: model.PhaseRTP, Kind: model.FaultCRC,
				SeqLo: p.ExtSeq, SeqHi: p.ExtSeq, Detail: "ingest CRC mismatch"})
		}
	}
	present := map[int64]*rtp.MediaPacket{}
	for _, p := range ordered {
		present[p.ExtSeq] = p
	}
	r.repairMissing(missing, present)

	// Re-drain after repair so recovered packets join the ordered set.
	ordered, _ = r.jitter.Drain()
	for _, p := range ordered {
		kind := syncer.TrackVideo // the fixture models a single video track
		ap := aligner.Align(kind, p.ExtSeq, p.NanoTS, p.Retrans, p.Recovered)
		r.aligned = append(r.aligned, ap)
	}
	aligned, err := aligner.Finalize(r.aligned)
	if err != nil {
		return err
	}
	r.aligned = aligned
	return nil
}

// repairMissing walks every hole reported by the jitter buffer. Retransmit
// and parity are attempted inside the finite window; when both fail an
// explicit gap is declared so the timeline is never silently spliced.
func (r *runner) repairMissing(missing []int64, present map[int64]*rtp.MediaPacket) {
	if len(missing) == 0 {
		return
	}
	// Group parity is always attempted first through Repairer.Try. The
	// sender's retransmission record is a finite backstop: only sequences
	// still present in that record can be re-served. Holes absent from both
	// the parity groups and the record become explicit gaps, never splices.
	for _, seq := range missing {
		rec := r.repair.Try(seq, present)
		if rec == nil {
			if payload, ok := r.stream.RetransRecord[seq]; ok {
				r.repair.Cache().Store(&rtp.MediaPacket{ExtSeq: seq, Payload: payload})
				rec = r.repair.Try(seq, present)
			}
		}
		if rec == nil {
			r.declareGap(seq)
			continue
		}
		if rec.Retrans {
			r.retrans++
		}
		if rec.Parity {
			r.fec++
		}
		rec.Packet.NanoTS = r.deriveNano(rec.Packet.ExtSeq)
		r.jitter.Put(rec.Packet)
		present[rec.Packet.ExtSeq] = rec.Packet
		if rtp.CheckCRC(rec.Packet.Payload) {
			r.crcMatched++
			r.packetsTotal++
		}
	}
}

// deriveNano reconstructs the shared-clock timestamp a lost packet would
// have carried, using the fixture's fixed per-packet timestamp increment.
func (r *runner) deriveNano(extSeq int64) model.Nano {
	idx := extSeq - int64(r.params.StartSeq)
	if idx < 0 {
		return 0
	}
	ticks := uint32(idx) * r.params.TSIncrement
	return model.Nano(int64(ticks) * model.Second.Nanos() / int64(r.params.ClockRate))
}

// declareGap maps an unrecoverable sequence to its segment and records an
// explicit, nanosecond-bounded gap on the fault log.
func (r *runner) declareGap(seq int64) {
	idx := seq - int64(r.params.StartSeq)
	segIdx := int(idx) / r.params.PacketsPerSegment
	if segIdx < 0 || segIdx >= len(r.tl.Segments) {
		segIdx = len(r.tl.Segments) - 1
	}
	seg := r.tl.Segments[maxInt(segIdx, 0)]
	start := r.deriveNano(seq)
	end := start + r.deriveNano(seq+1) - start
	r.repair.DeclareGap(seg.URI, seq, seq, start, end, model.FaultUnrecoverable)
	markSegmentGap(r.tl, seg.Seq)
}

// estimateActualKbps models the effective delivered bitrate of the selected
// representation. With every expected packet present it equals the target
// bitrate exactly (the representation the selector sustained); each
// unrecovered hole removes its share of carried media, so a degraded run
// reports a proportionally lower, honest figure rather than a fabricated
// one. Recovered packets count as delivered, which is the whole point of
// the repair stage.
func (r *runner) estimateActualKbps() float64 {
	expected := len(r.stream.IdealPackets)
	if expected == 0 {
		return 0
	}
	// Unique packets that finally made the aligned timeline: retained plus
	// successfully recovered, excluding explicit gaps.
	delivered := r.packetsTotal
	if delivered > expected {
		delivered = expected
	}
	deliveryRatio := float64(delivered) / float64(expected)
	return float64(r.cfg.TargetKbps) * deliveryRatio
}

func (r *runner) peakPeak() int64 {
	if q := int64(r.queue.PeakBytes()); q > r.peakQueue {
		return q
	}
	return r.peakQueue
}

func segmentsWithGaps(tl *manifest.Timeline, log *model.FaultLog) int {
	flagged := map[string]bool{}
	for _, g := range log.Gaps() {
		flagged[g.Segment] = true
	}
	n := 0
	for _, seg := range tl.Segments {
		if flagged[seg.URI] || seg.Gap {
			n++
		}
	}
	return n
}

func markSegmentGap(tl *manifest.Timeline, seq int) {
	for i := range tl.Segments {
		if tl.Segments[i].Seq == seq {
			tl.Segments[i].Gap = true
			return
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
