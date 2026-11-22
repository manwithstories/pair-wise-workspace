package rtp

import (
	"fmt"
	"io"
	"sort"

	pionrtp "github.com/pion/rtp"

	"mediabridge/internal/clock"
	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

// StreamStats summarises the damage found while reordering a dump.
type StreamStats struct {
	Received   int64
	Delivered  int64
	Duplicates int64
	Reordered  int64
	Lost       int64
	Resynced   int64
	FirstSeq   uint16
	LastSeq    uint16
	FirstTime  int64 // nanoseconds on the asset timeline
	LastTime   int64
}

// RecoveryRate is delivered/(delivered+lost), clamped to 1.
func (s StreamStats) RecoveryRate() float64 {
	total := s.Delivered + s.Lost
	if total <= 0 {
		return 1
	}
	return float64(s.Delivered) / float64(total)
}

// Options control demux of the auxiliary stream.
type Options struct {
	// SampleRate is the RTP clock rate (e.g. 90000).
	SampleRate uint32
	// TrackID overrides the SSRC as the track id when non-zero.
	TrackID uint32
	// MaxPackets caps delivered packets.
	MaxPackets int64
	// ReorderWindow bounds how many packets may be buffered awaiting a gap.
	ReorderWindow int
	// KeepPayload copies payload bytes into packets.
	KeepPayload bool
}

// Result is the demuxed auxiliary track.
type Result struct {
	Track   media.Track
	Packets []media.Packet
	Stats   StreamStats
}

// Demux reads the dump and produces a reordered auxiliary track.
func Demux(rd *Reader, opts Options) (*Result, error) {
	if opts.SampleRate == 0 {
		opts.SampleRate = 90000
	}
	if opts.MaxPackets <= 0 {
		opts.MaxPackets = clock.MaxPacketsPerTrack
	}
	if opts.ReorderWindow <= 0 {
		opts.ReorderWindow = 512
	}
	scale, err := clock.NewScale(opts.SampleRate)
	if err != nil {
		return nil, fault.New(fault.CodeTimescale, fault.Location{}, "rtp: invalid sample rate").Wrap(err)
	}

	trackID := opts.TrackID
	if trackID == 0 {
		trackID = rd.SSRC()
	}
	loc := func(off int64) fault.Location {
		return fault.Location{Adapter: "rtp", AssetID: trackIDString(trackID)}.
			WithOffset(off).WithTrack(trackID)
	}

	rdr := newReorderer(opts.ReorderWindow)
	var lastOffset int64
	for {
		rec, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fault.New(fault.CodeCorruptContainer, loc(lastOffset),
				"rtp dump corrupt: %v", err).WithOp("rtp.Demux")
		}
		lastOffset = rec.Offset
		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(rec.Payload); err != nil {
			return nil, fault.New(fault.CodeCorruptContainer,
				loc(rec.Offset).WithSequence(uint32(rec.Index)),
				"invalid rtp packet: %v", err).WithOp("rtp.Demux")
		}
		if err := rdr.push(&pkt, rec.Offset); err != nil {
			return nil, err
		}
	}
	delivered := rdr.drain()

	if len(delivered) == 0 {
		return nil, fault.New(fault.CodeNoPackets, loc(0),
			"rtp dump contains no packets").WithOp("rtp.Demux")
	}
	if int64(len(delivered)) > opts.MaxPackets {
		return nil, fault.New(fault.CodeDurationRange, loc(0),
			"rtp track has %d packets exceeding limit %d", len(delivered), opts.MaxPackets).
			WithOp("rtp.Demux")
	}

	res := &Result{Stats: rdr.stats}
	res.Stats.Delivered = int64(len(delivered))
	res.Stats.FirstSeq = delivered[0].seq
	res.Stats.LastSeq = delivered[len(delivered)-1].seq

	// Assign a monotonic 64-bit timestamp per delivered packet, then normalise
	// so the first packet starts at t=0. Timestamp deltas that go backwards
	// are clamped to zero so presentation stays monotonic.
	var ext uint32
	for i, d := range delivered {
		if i == 0 {
			ext = d.ts
		} else {
			delta := clock.SignedDelta32(delivered[i-1].ts, d.ts)
			if delta < 0 {
				delta = 0
				res.Stats.Reordered++
			}
			ext += uint32(delta)
		}
		d.absTS = ext
		delivered[i] = d
	}
	firstNS, _ := scale.Nanos(int64(delivered[0].absTS))

	prevNS := firstNS
	var lastEnd int64
	for i, d := range delivered {
		ns, err := scale.Nanos(int64(d.absTS))
		if err != nil {
			return nil, fault.New(fault.CodeTimestampWrap, loc(d.offset),
				"packet timestamp overflows: %v", err).WithOp("rtp.Demux")
		}
		var dur int64
		if i+1 < len(delivered) {
			nextNS, _ := scale.Nanos(int64(delivered[i+1].absTS))
			dur = nextNS - ns
		} else {
			dur = ns - prevNS
		}
		if dur < 0 {
			dur = 0
		}
		prevNS = ns
		p := media.Packet{
			TrackIndex:    0,
			SourceTrackID: trackID,
			PacketIndex:   int64(i),
			SourceOffset:  d.offset,
			SourceSize:    int64(len(d.payload)),
			DTSNS:         ns - firstNS,
			PTSNS:         ns - firstNS,
			DurationNS:    dur,
			Sync:          true,
			Sequence:      uint32(d.seq),
		}
		if end := p.PTSNS + p.DurationNS; end > lastEnd {
			lastEnd = end
		}
		if opts.KeepPayload {
			p.Payload = append([]byte(nil), d.payload...)
			p.Digest()
		}
		res.Packets = append(res.Packets, p)
	}
	res.Stats.FirstTime = 0
	res.Stats.LastTime = prevNS - firstNS

	res.Track = media.Track{
		Index:         0,
		ID:            trackID,
		Kind:          media.KindAux,
		Timescale:     opts.SampleRate,
		TimescaleName: "rtp.clock",
		OriginNS:      firstNS,
		DurationNS:    lastEnd,
		Codec:         media.Codec{SampleEntry: "rtp"},
		PacketCount:   int64(len(res.Packets)),
	}
	res.Track.Codec.Seal()
	return res, nil
}

func trackIDString(id uint32) string { return fmt.Sprintf("rtp-%d", id) }

// wire is one decoded packet with its dump offset.
type wire struct {
	seq     uint16
	ts      uint32
	payload []byte
	offset  int64
	absTS   uint32
}

// reorderer buffers out-of-order packets and releases a contiguous sequence
// prefix. Sequence arithmetic is done in int16 space so the 2^16 wrap is
// handled naturally.
type reorderer struct {
	window    int
	started   bool
	next      uint16
	pending   map[uint16]wire
	delivered []wire
	seen      map[uint16]bool
	stats     StreamStats
}

func newReorderer(window int) *reorderer {
	return &reorderer{
		window:  window,
		pending: make(map[uint16]wire),
		seen:    make(map[uint16]bool),
	}
}

func (r *reorderer) push(pkt *pionrtp.Packet, offset int64) error {
	r.stats.Received++
	seq := pkt.SequenceNumber
	if r.seen[seq] {
		r.stats.Duplicates++
		return nil
	}
	if _, dup := r.pending[seq]; dup {
		r.stats.Duplicates++
		return nil
	}
	r.seen[seq] = true

	w := wire{seq: seq, ts: pkt.Timestamp, payload: append([]byte(nil), pkt.Payload...), offset: offset}
	if !r.started {
		r.started = true
		r.next = seq
	}
	if int16(seq-r.next) < 0 {
		// Arrived after its slot was passed. Deliver at the end; it is still
		// better than dropping real payload, and it keeps hashing exact.
		r.stats.Reordered++
		r.delivered = append(r.delivered, w)
		return nil
	}
	if seq != r.next {
		// Ahead of the cursor with a gap: this packet overtook the ones still
		// missing, so it arrived out of order.
		r.stats.Reordered++
	}
	r.pending[seq] = w

	// Release the contiguous prefix.
	for {
		nw, ok := r.pending[r.next]
		if !ok {
			break
		}
		delete(r.pending, r.next)
		r.delivered = append(r.delivered, nw)
		r.next++
	}
	// Enforce the reorder window: if too much is buffered, declare the gap
	// lost and jump to the lowest buffered sequence ahead of next.
	if len(r.pending) > r.window {
		r.resync()
	}
	return nil
}

// resync declares the missing range lost and advances next to the lowest
// buffered sequence, then releases the newly contiguous prefix.
func (r *reorderer) resync() {
	var lowest uint16
	found := false
	for seq := range r.pending {
		if int16(seq-r.next) > 0 {
			if !found || int16(seq-lowest) < 0 {
				lowest, found = seq, true
			}
		}
	}
	if !found {
		return
	}
	gap := lowest - r.next
	r.stats.Lost += int64(gap)
	r.stats.Resynced++
	r.next = lowest
	for {
		nw, ok := r.pending[r.next]
		if !ok {
			break
		}
		delete(r.pending, r.next)
		r.delivered = append(r.delivered, nw)
		r.next++
	}
}

// drain flushes remaining buffered packets in ascending sequence order and
// counts any trailing gap as lost.
func (r *reorderer) drain() []wire {
	if len(r.pending) == 0 {
		return r.delivered
	}
	seqs := make([]uint16, 0, len(r.pending))
	for seq := range r.pending {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return int16(seqs[i]-seqs[j]) < 0 })
	// Any sequence between the current cursor and the first buffered packet was
	// never delivered: record it as loss before flushing.
	if first := seqs[0]; int16(first-r.next) > 0 {
		r.stats.Lost += int64(first - r.next)
		r.stats.Resynced++
	}
	prev := seqs[0]
	for _, seq := range seqs {
		if int16(seq-prev) > 1 {
			r.stats.Lost += int64(seq - prev - 1)
		}
		r.delivered = append(r.delivered, r.pending[seq])
		delete(r.pending, seq)
		prev = seq
	}
	return r.delivered
}
