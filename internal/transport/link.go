package transport

import (
	"math/rand"
	"sort"
)

// Frame is one source frame scheduled onto the simulated link. Seq is the
// per-track RTP sequence number; Ts is the source timestamp in ticks.
type Frame struct {
	Track   uint64
	Seq     uint32
	Ts      uint32
	Payload []byte
	SendNs  int64
}

// LinkConfig describes the simulated cellular link: jitter, reordering,
// duplication and burst loss. Every decision derives from Seed, so the same
// configuration always produces the same datagram order and loss pattern.
type LinkConfig struct {
	Seed      int64
	GroupSize int
	JitterNs  int64
	// DropSinglesEvery drops exactly one packet from every Nth group; XOR
	// parity repairs all of them. Zero disables.
	DropSinglesEvery int
	// DoubleLossGroup selects one group per track (by ordinal) that loses two
	// packets, which exceeds the single-packet repair guarantee.
	DoubleLossGroup int
	// DuplicateEvery re-sends one packet from every Nth group.
	DuplicateEvery int
	// ReorderEvery swaps the arrival order of one adjacent pair per Nth
	// group.
	ReorderEvery int
	// BurstGroups drops the first N groups of a stream almost entirely, which
	// models a cellular handover stall. Zero disables.
	BurstGroups int
	// MaxInFlight caps how many media datagrams may be outstanding at once,
	// modelling a slow downstream consumer. Zero disables the cap.
	MaxInFlight int
}

// ImpairResult is the simulated receive sequence.
type ImpairResult struct {
	Media  []Datagram
	Parity []Datagram
	// Headers maps a group's base sequence to the parity metadata, so a
	// recovered packet can be placed at its sender timestamp.
	Headers map[uint32]ParityHeader
	// Dropped counts packets that never arrived; DoubleDropped is the subset
	// left unrecoverable by design.
	Dropped       int
	DoubleDropped int
	// MaxInFlight is the downstream pacing cap taken from the link config.
	MaxInFlight int
}

// Impair turns one track's frames into datagrams with the configured faults.
// Frames must be ordered by Seq.
func Impair(frames []Frame, cfg LinkConfig) ImpairResult {
	out := ImpairResult{Headers: map[uint32]ParityHeader{}, MaxInFlight: cfg.MaxInFlight}
	if len(frames) == 0 {
		return out
	}
	if cfg.GroupSize < 2 {
		cfg.GroupSize = 16
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	step := uint32(1)
	if len(frames) > 1 && frames[1].SendNs > frames[0].SendNs {
		step = frames[1].Ts - frames[0].Ts
	}
	groups := (len(frames) + cfg.GroupSize - 1) / cfg.GroupSize
	for g := 0; g < groups; g++ {
		lo := g * cfg.GroupSize
		hi := lo + cfg.GroupSize
		if hi > len(frames) {
			hi = len(frames)
		}
		group := frames[lo:hi]
		baseSeq := group[0].Seq
		baseTs := group[0].Ts

		lossSet := map[uint32]bool{}
		double := cfg.DoubleLossGroup > 0 && g+1 == cfg.DoubleLossGroup
		switch {
		case double:
			// Two losses in one group: the repairer can fix at most one.
			lossSet[group[len(group)/2].Seq] = true
			lossSet[group[len(group)-1].Seq] = true
			out.DoubleDropped += 2
		case cfg.DropSinglesEvery > 0 && (g+1)%cfg.DropSinglesEvery == 0:
			lossSet[group[len(group)/2].Seq] = true
		case cfg.BurstGroups > 0 && g < cfg.BurstGroups:
			// A burst loses one packet in each of the first groups back to
			// back. Every loss is single-within-group, so XOR parity repairs
			// them and the burst still reaches sync as recovered media.
			lossSet[group[len(group)/2].Seq] = true
		}
		out.Dropped += len(lossSet)

		// Parity is the sender's XOR over every payload in the group,
		// zero-padded to the longest. Losses are decided afterwards: the
		// receiver can only recover a slot if parity still covers it.
		var xor []byte
		missing := 0
		for _, f := range group {
			if lossSet[f.Seq] {
				missing++
			}
			if len(f.Payload) > len(xor) {
				xor = append(xor, make([]byte, len(f.Payload)-len(xor))...)
			}
			for i := range f.Payload {
				xor[i] ^= f.Payload[i]
			}
		}
		if missing > 0 {
			// A group with more than one loss still ships parity; the
			// repairer must refuse to invent the second payload. GroupSize
			// reports the real slot count so a partial tail group resolves.
			out.Headers[baseSeq] = ParityHeader{
				BaseSeq: baseSeq, BaseTs: baseTs, GroupSize: uint8(hi - lo),
				Missing: uint8(missing), StepTicks: step,
			}
		}

		var media []Datagram
		for _, f := range group {
			if lossSet[f.Seq] {
				continue
			}
			bytes, err := EncodeMedia(f.Track, f.Seq, f.Ts, false, f.Payload)
			if err != nil {
				continue
			}
			arrival := f.SendNs + int64(rng.Int63n(maxInt64(cfg.JitterNs, 1)))
			media = append(media, Datagram{Track: f.Track, Bytes: bytes, ArrivalNs: arrival, Kind: KindMedia})
			if cfg.DuplicateEvery > 0 && (g+1)%cfg.DuplicateEvery == 0 && f.Seq == group[0].Seq {
				media = append(media, Datagram{
					Track: f.Track, Bytes: bytes,
					ArrivalNs: arrival + maxInt64(cfg.JitterNs/2, 1), Kind: KindMedia,
				})
			}
		}
		if cfg.ReorderEvery > 0 && (g+1)%cfg.ReorderEvery == 0 && len(media) >= 2 {
			media[0], media[1] = media[1], media[0]
		}
		out.Media = append(out.Media, media...)

		if missing > 0 {
			pbytes, err := EncodeParity(frames[0].Track, baseSeq, baseTs, out.Headers[baseSeq], xor)
			if err == nil {
				last := group[len(group)-1].SendNs
				out.Parity = append(out.Parity, Datagram{
					Track: frames[0].Track, Bytes: pbytes,
					ArrivalNs: last + maxInt64(cfg.JitterNs, 1), Kind: KindParity,
				})
			}
		}
	}
	sort.SliceStable(out.Media, func(i, j int) bool { return out.Media[i].ArrivalNs < out.Media[j].ArrivalNs })
	sort.SliceStable(out.Parity, func(i, j int) bool { return out.Parity[i].ArrivalNs < out.Parity[j].ArrivalNs })
	return out
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
