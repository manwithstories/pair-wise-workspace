package pipeline

import (
	"sort"

	"mediamix/internal/audio"
	"mediamix/internal/codec"
	"mediamix/internal/core"
	"mediamix/internal/fixture"
	"mediamix/internal/timeline"
	"mediamix/internal/transport"
)

// runTrack drives one track through reorder, recovery, index validation and
// (for audio) normalization. Every failure is captured in the result instead of
// propagating, which is what keeps other tracks independent.
func runTrack(fx *fixture.Fixture, node *timeline.Node, engine *transport.RecoveryEngine, opts Options) *TrackResult {
	res := &TrackResult{Track: node.Track, Kind: node.Kind, Status: StatusOK}
	spec := findSpec(fx, node.Track)
	if spec == nil {
		res.fail(&core.Error{Code: core.CodeInternal, Track: node.Track, Detail: "fixture missing track spec", Fatal: true})
		return res
	}

	// Validate the keyframe index before transport work: a track whose index
	// cannot be trusted must fail loudly even if its packets all arrive.
	var idx *codec.Index
	if node.Kind == timeline.KindVideo {
		var verr error
		idx, verr = validateVideoIndex(fx, spec, res)
		if verr != nil {
			res.fail(core.StampTrack(verr, node.Track))
			return res
		}
		res.Index = idx
	}

	deliveries := fixture.Impair(fx.Sent[node.Track], fixture.NetworkOptions{
		LossBurst: opts.LossBurst, BandwidthMBps: opts.BandwidthMBps,
		Seed: opts.Seed, DuplicateEvery: 11,
	})
	buf, err := transport.NewReorderBuffer(node.Track, transport.Limits{MaxBuffered: engine.BufferBound()})
	if err != nil {
		res.fail(core.StampTrack(err, node.Track))
		return res
	}

	// Feed arrivals in time order: the arrival clock is the receiver's. Each
	// sender packet is serialized as a real RTP datagram and decoded again, so
	// ingest exercises the same byte path the deployment uses; the reorder
	// buffer then sees the sequence number from that wire header.
	//
	// When a hole blocks delivery, retransmission is attempted immediately; if
	// it cannot be repaired the hole is skipped and the track continues alone.
	sort.SliceStable(deliveries, func(i, j int) bool { return deliveries[i].ArrivalUs < deliveries[j].ArrivalUs })
	wire := transport.NewWire()
	var delivered []transport.Packet
	var lostGaps []transport.Gap

	for _, d := range deliveries {
		if d.Dropped {
			continue
		}
		datagram, werr := wire.Encode(toPacket(node.Track, d.Packet, d.ArrivalUs))
		if werr != nil {
			res.fail(core.StampTrack(werr, node.Track))
			return res
		}
		p, werr := wire.Decode(node.Track, datagram, d.ArrivalUs)
		if werr != nil {
			res.fail(core.StampTrack(werr, node.Track))
			return res
		}
		p.Keyframe = d.Packet.Keyframe
		pkts, perr := buf.Push(p, d.ArrivalUs)
		if perr != nil && core.CodeOf(perr) != core.CodeOverLimit {
			res.fail(core.StampTrack(perr, node.Track))
			return res
		}
		delivered = append(delivered, pkts...)
		if g, ok := buf.OpenGap(); ok {
			healed, outcome, _ := engine.RepairGap(buf, d.ArrivalUs)
			delivered = append(delivered, healed...)
			if outcome == transport.Unrecoverable {
				lostGaps = append(lostGaps, g)
				skipped, _ := buf.Advance()
				delivered = append(delivered, skipped...)
			}
		}
	}

	// Final repair attempt and flush at the end of the stream.
	if g, ok := buf.OpenGap(); ok {
		healed, outcome, _ := engine.RepairGap(buf, lastArrival(deliveries))
		delivered = append(delivered, healed...)
		if outcome == transport.Unrecoverable {
			lostGaps = append(lostGaps, g)
		}
	}
	tail, tailSkipped := buf.Flush()
	delivered = append(delivered, tail...)
	if tailSkipped > 0 && len(lostGaps) == 0 {
		// Defensive: Flush can skip a residue not recorded above.
		lostGaps = append(lostGaps, transport.Gap{Track: node.Track, Count: tailSkipped})
	}

	sort.SliceStable(delivered, func(i, j int) bool { return delivered[i].Ext < delivered[j].Ext })

	st := buf.Stats()
	rep := engine.Report(node.Track)
	res.Received = st.Received
	res.Delivered = len(delivered)
	res.Duplicates = st.Duplicates
	res.Reordered = st.Reordered
	res.Recovered = rep.Recovered
	res.Unrecoverable = len(lostGaps)
	res.SkippedPackets = rep.AbandonedPkts
	res.LatencyP95Ms = p95Latency(deliveries, delivered)
	if len(lostGaps) > 0 {
		res.Status = StatusDegraded
		res.Affected = affectedSpans(fx, lostGaps, spec, idx)
		res.Adjustments = append(res.Adjustments, &core.Error{
			Code: core.CodeUnrecoverable, Track: node.Track,
			Detail: "unrecovered gaps mark affected spans; independent tracks continue",
			Fatal:  false,
		})
	}

	// Effective sync offset is the median delay between a packet's intended
	// presentation time and its arrival. The median makes the figure robust
	// against reordering jitter; the spread of these medians across tracks is
	// the A/V drift the report exposes.
	if off, ok := medianMediaOffset(deliveries, delivered); ok {
		res.SyncOffsetUs = off
	}

	if node.Kind == timeline.KindVideo {
		return res
	}
	return runAudio(fx, node, opts, res)
}

// runAudio normalizes one audio track to the unified deliverable spec.
func runAudio(fx *fixture.Fixture, node *timeline.Node, opts Options, res *TrackResult) *TrackResult {
	mat := fx.Audio[node.Track]
	if mat == nil {
		res.fail(&core.Error{Code: core.CodeEmptyTrack, Track: node.Track, Detail: "audio material missing", Fatal: true})
		return res
	}
	am := &audio.Material{Track: mat.Track, Rate: mat.Rate, Layout: audio.Layout(mat.Layout), Span: mat.Span, Samples: mat.Samples}
	ares, aerr := audio.Normalize(am, opts.Target)
	if aerr != nil {
		res.fail(core.StampTrack(aerr, node.Track))
		return res
	}
	res.Audio = ares
	for _, s := range ares.Status {
		switch s.Code {
		case core.CodeSilentAudio, core.CodeClippedAudio, core.CodeEmptyTrack:
			if res.Status == StatusOK {
				res.Status = StatusDegraded
			}
		}
		res.Adjustments = append(res.Adjustments, s)
	}
	// Truncated audio keeps its declared span but loses frames; report the
	// mismatch rather than pretending the deliverable is complete.
	if opts.TruncateTrack == node.Track {
		res.Status = StatusDegraded
		res.Adjustments = append(res.Adjustments, &core.Error{
			Code: core.CodeAudioInvalid, Track: node.Track,
			Detail: "input truncated: sample count does not cover declared span", Fatal: false,
		})
	}
	return res
}

// affectedSpans converts lost sequence gaps into media spans. For video the
// spans are widened to the enclosing sync samples, because a damaged span is
// only decodable from one keyframe to the next.
func affectedSpans(fx *fixture.Fixture, gaps []transport.Gap, spec *fixture.TrackSpec, idx *codec.Index) []timeline.Span {
	sent := fx.Sent[spec.ID]
	out := make([]timeline.Span, 0, len(gaps))
	for _, g := range gaps {
		start := spanStart(sent, spec, g.StartExt)
		end := spanEnd(sent, spec, g.EndExt)
		if end <= start {
			end = start + spec.SegmentLen
		}
		span := timeline.Span{Start: start, End: end}
		if idx != nil {
			span = widenToKeyframes(span, idx)
		}
		out = append(out, span)
	}
	return timeline.MergeSpans(out)
}

func spanStart(sent []fixture.SentPacket, spec *fixture.TrackSpec, ext uint64) timeline.Micros {
	if int(ext) < len(sent) {
		return sent[ext].SentUs
	}
	return spec.Origin + spec.DurationUs
}

func spanEnd(sent []fixture.SentPacket, spec *fixture.TrackSpec, ext uint64) timeline.Micros {
	next := int(ext) + 1
	if next < len(sent) {
		return sent[next].SentUs
	}
	return spec.Origin + spec.DurationUs
}

// widenToKeyframes expands a damaged interval out to the nearest sync samples.
func widenToKeyframes(span timeline.Span, idx *codec.Index) timeline.Span {
	start := span.Start
	if s, ok := idx.Seek(span.Start); ok {
		start = s.PTS
	}
	end := span.End
	for _, s := range idx.Sync {
		if s.PTS >= span.End {
			end = s.PTS
			break
		}
	}
	if end < start {
		end = start
	}
	return timeline.Span{Start: start, End: end}
}

// validateVideoIndex checks the keyframe index against the generated stream.
func validateVideoIndex(fx *fixture.Fixture, spec *fixture.TrackSpec, res *TrackResult) (*codec.Index, error) {
	stream := fx.Streams[spec.ID]
	base, err := timeline.NewTimeBase(spec.Timescale)
	if err != nil {
		return nil, err
	}
	base.Origin = spec.Origin
	entries := fx.Indexes[spec.ID]
	samples := make([]codec.Sample, len(entries))
	for i, k := range entries {
		samples[i] = codec.Sample{Offset: int64(k.OffsetUs), Size: k.Size, PTS: k.PTS, Key: k.Key}
	}
	idx, err := codec.Verify(spec.ID, codec.Format(spec.Format), stream, base, samples)
	if err != nil {
		return nil, err
	}
	res.Adjustments = append(res.Adjustments, idx.Rejects...)
	return idx, nil
}

// medianMediaOffset returns the median (arrival - intended presentation) over
// delivered packets. Positive means media is late relative to its own clock.
func medianMediaOffset(deliveries []fixture.Delivery, delivered []transport.Packet) (timeline.Micros, bool) {
	if len(delivered) == 0 {
		return 0, false
	}
	bySeq := make(map[uint16]fixture.Delivery, len(deliveries))
	for _, d := range deliveries {
		bySeq[d.Packet.Seq] = d
	}
	offs := make([]timeline.Micros, 0, len(delivered))
	for _, p := range delivered {
		d, ok := bySeq[p.Seq]
		if !ok {
			continue
		}
		offs = append(offs, d.ArrivalUs-d.Packet.SentUs)
	}
	if len(offs) == 0 {
		return 0, false
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	return offs[len(offs)/2], true
}

// p95Latency estimates the 95th percentile receiver latency: arrival minus the
// sender's intended presentation time, clamped at zero. Retransmitted packets
// contribute one recovery deadline, which is what they actually cost.
func p95Latency(deliveries []fixture.Delivery, delivered []transport.Packet) float64 {
	if len(delivered) == 0 {
		return 0
	}
	bySeq := make(map[uint16]fixture.Delivery, len(deliveries))
	for _, d := range deliveries {
		bySeq[d.Packet.Seq] = d
	}
	lat := make([]float64, 0, len(delivered))
	for _, p := range delivered {
		if d, ok := bySeq[p.Seq]; ok {
			delta := float64(d.ArrivalUs-d.Packet.SentUs) / 1000
			if delta < 0 {
				delta = 0
			}
			lat = append(lat, delta)
			continue
		}
		if p.Retransmit {
			lat = append(lat, transport.DefaultRecoveryPolicy().DeadlineMs)
		}
	}
	if len(lat) == 0 {
		return 0
	}
	sort.Float64s(lat)
	return percentile(lat, 0.95)
}

func toPacket(track string, p fixture.SentPacket, arrival timeline.Micros) transport.Packet {
	return transport.Packet{
		Track: track, Seq: p.Seq, Timestamp: p.Timestamp,
		Marker: p.Marker, Keyframe: p.Keyframe, Payload: p.Payload, ArrivalUs: arrival,
	}
}

func lastArrival(deliveries []fixture.Delivery) timeline.Micros {
	var last timeline.Micros
	for _, d := range deliveries {
		if d.ArrivalUs > last {
			last = d.ArrivalUs
		}
	}
	return last
}

func findSpec(fx *fixture.Fixture, id string) *fixture.TrackSpec {
	for i := range fx.Tracks {
		if fx.Tracks[i].ID == id {
			return &fx.Tracks[i]
		}
	}
	return nil
}

// fail marks a result failed with the given error.
func (r *TrackResult) fail(err error) {
	e, ok := core.As(err)
	if !ok {
		e = &core.Error{Code: core.CodeInternal, Track: r.Track, Detail: err.Error(), Fatal: true}
	}
	if e.Track == "" {
		e.Track = r.Track
	}
	r.Status = StatusFailed
	r.Error = e
}
