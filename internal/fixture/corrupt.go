package fixture

import (
	"math/rand"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// truncate cuts a track's stream short and leaves its index pointing past the
// new end. This is the "truncate-track" fault used by the demo: the index is
// now out of range and must be rejected, not silently clipped.
func (f *Fixture) truncate(track string) error {
	stream, ok := f.Streams[track]
	if !ok {
		if m, ok := f.Audio[track]; ok && m != nil {
			// Audio truncation: drop trailing samples but keep the declared
			// span, so duration mismatch is visible to the pipeline.
			if len(m.Samples) > 0 {
				cut := len(m.Samples) / 2
				m.Samples = m.Samples[:cut]
			}
			f.Corrupt[track] = "truncated audio samples"
			return nil
		}
		return &core.Error{Code: core.CodeBadArgument, Track: track, Detail: "truncate target is not a generated track", Fatal: true}
	}
	if len(stream) == 0 {
		return &core.Error{Code: core.CodeEmptyTrack, Track: track, Detail: "cannot truncate an empty stream", Fatal: true}
	}
	cut := len(stream) * 2 / 3
	f.Streams[track] = stream[:cut]
	f.Corrupt[track] = "stream truncated; index extends past end"
	return nil
}

// corruptIndex deliberately breaks one video index so the demo shows an index
// rejection with the affected track named. The first keyframe's offset is moved
// one byte past the end of its stream, which is the classic out-of-range case.
func (f *Fixture) corruptIndex(rng *rand.Rand) {
	for _, spec := range f.Tracks {
		if spec.Kind != timeline.KindVideo {
			continue
		}
		idx := f.Indexes[spec.ID]
		if len(idx) < 2 {
			continue
		}
		streamEnd := timeline.Micros(len(f.Streams[spec.ID]))
		idx[1].OffsetUs = streamEnd + timeline.Micros(1+rng.Intn(4096))
		f.Indexes[spec.ID] = idx
		f.Corrupt[spec.ID] = "keyframe index offset beyond stream end"
		return
	}
}

// NetworkOptions configure the simulated uplink impairment.
type NetworkOptions struct {
	LossBurst    int
	BandwidthMBps float64
	Seed         int64
	// ReorderWindow reorders arrivals within this many packets.
	ReorderWindow int
	// DuplicateEvery duplicates every Nth packet (0 disables duplication).
	DuplicateEvery int
}

// Delivery is one packet as it arrives at the receiver. ArrivalUs is the
// receiver's clock; DueUs is the sender's intended presentation time.
type Delivery struct {
	Packet   SentPacket
	ArrivalUs timeline.Micros
	Dropped  bool
}

// Impair applies loss bursts, reordering and duplication to a sent sequence.
// The result is deterministic for a given seed and is what the pipeline feeds
// into the reorder buffer.
func Impair(sent []SentPacket, opts NetworkOptions) []Delivery {
	rng := rand.New(rand.NewSource(opts.Seed ^ 0x5eed))
	out := make([]Delivery, 0, len(sent))
	if len(sent) == 0 {
		return out
	}
	if opts.ReorderWindow <= 0 {
		opts.ReorderWindow = 4
	}
	// Choose burst starts: one burst per ~60 packets, each burst of LossBurst.
	drops := make(map[int]bool)
	if opts.LossBurst > 0 {
		bursts := len(sent)/60 + 1
		for b := 0; b < bursts; b++ {
			start := rng.Intn(len(sent))
			for i := 0; i < opts.LossBurst; i++ {
				drops[(start+i)%len(sent)] = true
			}
		}
		// Never drop the very first packet of a track: without it the stream
		// has no origin and the whole track would be unattributable.
		delete(drops, 0)
	}
	jitter := timeline.Micros(0)
	if opts.BandwidthMBps > 0 && opts.BandwidthMBps < 5 {
		// Low bandwidth stretches spacing: 1ms per packet per missing Mbps.
		jitter = timeline.Micros((5 - opts.BandwidthMBps) * 2000)
	}
	for i, p := range sent {
		d := Delivery{Packet: p}
		if drops[i] {
			d.Dropped = true
			out = append(out, d)
			continue
		}
		arrival := p.SentUs + jitter*timeline.Micros(i%8)
		// Reorder: swap arrival with a later neighbour occasionally.
		if i > 0 && i%17 == 0 {
			arrival -= timeline.Micros(opts.ReorderWindow) * 1000
		}
		if arrival < 0 {
			arrival = 0
		}
		d.ArrivalUs = arrival
		out = append(out, d)
		if opts.DuplicateEvery > 0 && i%opts.DuplicateEvery == 0 {
			dup := d
			dup.ArrivalUs = arrival + 1500
			out = append(out, dup)
		}
	}
	return out
}

// SenderFromDeliveries builds the retransmit source used by the recovery
// engine. Only dropped packets that were actually sent can be fetched.
func SenderFromDeliveries(deliveries []Delivery) *Sender {
	byTrack := make(map[string][]SentPacket)
	for _, d := range deliveries {
		byTrack[d.Packet.Track] = append(byTrack[d.Packet.Track], d.Packet)
	}
	return &Sender{byTrack: byTrack}
}

// Sender is a deterministic local stand-in for an RTX peer.
type Sender struct {
	byTrack map[string][]SentPacket
}

// Fetch implements transport.RecoverySource.
func (s *Sender) Fetch(track string, ext uint64) (SentPacket, bool) {
	list := s.byTrack[track]
	if int(ext) >= len(list) {
		return SentPacket{}, false
	}
	p := list[ext]
	return p, true
}

// FailedRecords returns the seeded loss count per track for diagnostics.
func FailedRecords(deliveries []Delivery) map[string]int {
	out := make(map[string]int)
	for _, d := range deliveries {
		if d.Dropped {
			out[d.Packet.Track]++
		}
	}
	return out
}
