package pipeline

import (
	"context"

	"avcheck/internal/ebml"
	"avcheck/internal/media"
	"avcheck/internal/timeline"
	"avcheck/internal/transport"
)

// align runs the transport and sync stages in dependency order. Container
// blocks are muxed onto a seeded RTP link; the jitter buffer re-orders and
// de-duplicates them, XOR parity repairs single losses, and whatever survives
// is aligned against the audio anchor. Nothing is synthesised: an unrecovered
// sequence becomes a counted gap, never a sample with an invented time.
func (r *Runner) align(ctx context.Context, doc *ebml.Document, graph *ebml.Graph) (*timeline.Result, error) {
	order, err := graph.FinishOrder()
	if err != nil {
		return nil, err
	}
	if _, err := graph.Anchor(); err != nil {
		return nil, err
	}
	streams, err := r.buildStreams(doc, graph, order)
	if err != nil {
		return nil, err
	}
	samples := make(map[uint64][]timeline.Sample, len(order))
	gaps := make(map[uint64][]uint32, len(order))
	var peak, payloadBytes int64
	var recovered, unrecovered, duplicates int

	defer func() {
		for _, s := range streams {
			s.buf.Close()
		}
	}()

	for _, s := range streams {
		received, err := r.driveStream(ctx, s, &peak, &payloadBytes)
		if err != nil {
			return nil, err
		}
		recSeqs := map[uint32]bool{}
		for _, seq := range s.rep.RecoveredSeqs() {
			recSeqs[seq] = true
		}
		// Every frame that never arrived is a candidate for repair; parity
		// fed above may already have resolved some of them.
		lost := make([]uint32, 0)
		for _, f := range s.frames {
			if _, ok := received[f.Seq]; ok {
				continue
			}
			if recSeqs[f.Seq] {
				continue
			}
			lost = append(lost, f.Seq)
		}
		r.emitSamples(doc, s, received, recSeqs, lost, samples, gaps)
		st := s.buf.Stats()
		duplicates += st.Duplicates
		recovered += s.rep.Recovered
		unrecovered += s.rep.Unrecovered
		if st.PeakBytes > peak {
			peak = st.PeakBytes
		}
	}

	res, err := timeline.Align(doc, graph, samples, gaps, r.cfg.Sync)
	if err != nil {
		return nil, err
	}
	res.PeakBufferBytes = peak
	res.PayloadBytes = payloadBytes
	res.RecoveredPackets = recovered
	res.UnrecoveredPackets = unrecovered
	res.Duplicates = duplicates
	res.RecoveredPct = 100
	if recovered+unrecovered > 0 {
		res.RecoveredPct = float64(recovered) * 100 / float64(recovered+unrecovered)
	}
	return res, nil
}

// driveStream pushes every datagram through the jitter buffer, flushes the
// remainder, marks losses and applies parity. It returns the received packets
// that survived ordering.
func (r *Runner) driveStream(ctx context.Context, s *trackStream, peak, payloadBytes *int64) (map[uint32]transport.Packet, error) {
	received := map[uint32]transport.Packet{}
	for _, d := range s.result.Media {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pkt, err := transport.DecodeMedia(d)
		if err != nil {
			r.fault.Addf(media.LevelDegraded, media.CodeInternal, s.track, 0, "%v", err)
			continue
		}
		// Slow downstream: when more than MaxInFlight packets are queued, the
		// consumer stalls the producer by only releasing packets whose jitter
		// deadline has arrived. This is true backpressure: the producer
		// waits, nothing is dropped and no extra memory is used.
		if cap := s.result.MaxInFlight; cap > 0 {
			for s.buf.Len() >= cap {
				drained := false
				for _, out := range s.buf.PopReady(s.buf.OldestDeadline()) {
					s.rep.Add(out)
					received[out.Seq] = out
					drained = true
				}
				if !drained {
					break
				}
			}
		}
		if err := s.buf.Push(pkt); err != nil {
			if err != transport.ErrBackpressure {
				continue
			}
			// Slow consumer: drain what has aged out to make room, then retry
			// once. A packet that still does not fit is a counted drop.
			for _, out := range s.buf.PopReady(pkt.ArrivalNs) {
				s.rep.Add(out)
				received[out.Seq] = out
			}
			if err := s.buf.Push(pkt); err != nil {
				r.fault.Addf(media.LevelDegraded, media.CodeSlowConsumer,
					s.track, int64(pkt.Seq), "packet dropped under backpressure")
				continue
			}
		}
		if st := s.buf.Stats(); st.PeakBytes > *peak {
			*peak = st.PeakBytes
		}
	}
	for _, pkt := range s.buf.Flush() {
		s.rep.Add(pkt)
		received[pkt.Seq] = pkt
	}
	// Trailing loss never reaches the buffer as a gap; mark it explicitly so
	// parity can still target it.
	for _, f := range s.frames {
		if _, ok := received[f.Seq]; !ok {
			s.rep.MarkLost(f.Seq)
		}
	}
	for _, seq := range s.buf.Missing() {
		s.rep.MarkLost(seq)
	}
	for _, d := range s.result.Parity {
		h, xor, err := transport.DecodeParity(d)
		if err != nil {
			r.fault.Addf(media.LevelDegraded, media.CodeRepairFailed, s.track, 0, "%v", err)
			continue
		}
		s.rep.FeedParity(h, xor)
	}
	groups := s.rep.Finish()
	for _, g := range groups {
		for _, seq := range g.RecoveredSeqs() {
			recovered, ok := s.rep.Payload(seq)
			if !ok || len(recovered) == 0 {
				continue
			}
			if _, ok := received[seq]; ok {
				continue
			}
			// Carry the sender-derived RTP timestamp so the recovered packet
			// lands at its real position on the timeline, not at tick zero.
			ts, _ := s.recoveredTicks(seq)
			received[seq] = transport.Packet{
				Stream: uint16(s.track), Seq: seq, Timestamp: ts, Payload: recovered,
			}
		}
	}
	for _, pkt := range received {
		*payloadBytes += int64(len(pkt.Payload))
	}
	if st := s.buf.Stats(); st.PeakBytes > *peak {
		*peak = st.PeakBytes
	}
	return received, nil
}
