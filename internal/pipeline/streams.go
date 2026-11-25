package pipeline

import (
	"sort"

	"avcheck/internal/ebml"
	"avcheck/internal/media"
	"avcheck/internal/timeline"
	"avcheck/internal/transport"
)

// emitSamples converts surviving and recovered packets into timeline samples,
// and records the remaining losses as gaps. Times come from the RTP timestamp
// converted with the shared rational clock; recovered packets use the parity
// header's base timestamp and per-slot tick step.
func (r *Runner) emitSamples(doc *ebml.Document, s *trackStream,
	received map[uint32]transport.Packet, recovered map[uint32]bool, lost []uint32,
	samples map[uint64][]timeline.Sample, gaps map[uint64][]uint32) {
	scale := media.Rational{Num: doc.TimecodeScale, Den: 1}
	if scale.Num <= 0 {
		r.fault.Addf(media.LevelAbort, media.CodeBadTrack, s.track, 0, "invalid timecode scale")
		return
	}
	// Iterate in sequence order so identical seeds produce identical fault
	// ordering and therefore identical manifest digests.
	seqs := make([]uint32, 0, len(received))
	for seq := range received {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for _, seq := range seqs {
		pkt := received[seq]
		ns := media.Nanos(int64(pkt.Timestamp), scale)
		samples[s.track] = append(samples[s.track], timeline.Sample{
			Track: s.track, Seq: seq, TimeNs: ns, SourceNs: ns,
			Recovered: recovered[seq],
		})
	}
	recs := make([]uint32, 0, len(recovered))
	for seq := range recovered {
		if _, ok := received[seq]; !ok {
			recs = append(recs, seq)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i] < recs[j] })
	for _, seq := range recs {
		ts, ok := s.recoveredTicks(seq)
		if !ok {
			r.fault.Addf(media.LevelDegraded, media.CodeRepairFailed, s.track, int64(seq),
				"recovered packet has no parity header for placement")
			continue
		}
		ns := media.Nanos(int64(ts), scale)
		samples[s.track] = append(samples[s.track], timeline.Sample{
			Track: s.track, Seq: seq, TimeNs: ns, SourceNs: ns, Recovered: true,
		})
	}
	gaps[s.track] = append(gaps[s.track], lost...)
}

// recoveredTicks derives a recovered packet's RTP timestamp from its group's
// base timestamp and per-slot tick step. Without a header the packet cannot be
// placed, which is reported rather than guessed.
func (s *trackStream) recoveredTicks(seq uint32) (uint32, bool) {
	bases := make([]uint32, 0, len(s.result.Headers))
	for base := range s.result.Headers {
		bases = append(bases, base)
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })
	for _, base := range bases {
		h := s.result.Headers[base]
		d := transport.SeqDiff(seq, base)
		if d < 0 || d >= int32(h.GroupSize) {
			continue
		}
		return uint32(int64(h.BaseTs) + int64(d)*int64(h.StepTicks)), true
	}
	return 0, false
}

// buildStreams muxes the document's blocks onto one simulated RTP link per
// track, seeded so a given seed always yields the same datagram sequence.
func (r *Runner) buildStreams(doc *ebml.Document, graph *ebml.Graph, order []uint64) ([]*trackStream, error) {
	byTrack := make(map[uint64][]ebml.Block, len(order))
	for _, b := range doc.Blocks {
		if doc.StoppedTracks[b.Track] {
			continue
		}
		byTrack[b.Track] = append(byTrack[b.Track], b)
	}
	out := make([]*trackStream, 0, len(order))
	for _, tnum := range order {
		blocks := byTrack[tnum]
		frames := make([]transport.Frame, 0, len(blocks))
		for i, b := range blocks {
			frames = append(frames, transport.Frame{
				Track: tnum, Seq: uint32(i), Ts: uint32(b.Ticks),
				Payload: b.Data, SendNs: b.TimeNs,
			})
		}
		kind := graph.Kind(tnum)
		cfg := r.linkFor(tnum, kind, frames)
		res := transport.Impair(frames, cfg)
		buf, err := transport.NewBuffer(uint16(tnum), r.cfg.Transport, r.fault)
		if err != nil {
			return nil, err
		}
		rep, err := transport.NewRepairer(uint16(tnum), r.cfg.Transport.GroupSize, r.fault)
		if err != nil {
			return nil, err
		}
		out = append(out, &trackStream{track: tnum, kind: kind, frames: frames, result: res, buf: buf, rep: rep})
	}
	return out, nil
}

// linkFor derives the per-track impairment profile. Audio is kept lossless so
// the anchor clock stays exact; loss is applied to video and cues, where a
// visible stall or misplacement is what the scenario reports.
func (r *Runner) linkFor(track uint64, kind media.TrackKind, frames []transport.Frame) transport.LinkConfig {
	cfg := r.cfg.Link
	cfg.Seed = r.cfg.Seed + int64(track)*7919
	if cfg.GroupSize <= 0 {
		cfg.GroupSize = r.cfg.Transport.GroupSize
	}
	if kind == media.KindAudio {
		cfg.DropSinglesEvery = 0
		cfg.DoubleLossGroup = 0
		cfg.DuplicateEvery = 0
		cfg.BurstGroups = 0
	}
	return cfg
}
