package testdata

import (
	"math/rand"
)

// Scenario selects which impairment class a stream exhibits.
type Scenario string

const (
	ScenarioClean       Scenario = "clean"
	ScenarioReorderDup  Scenario = "reorder_dup"
	ScenarioBurstLoss   Scenario = "burst_loss"
	ScenarioTruncated   Scenario = "truncated"
	ScenarioCorruptRTCP Scenario = "corrupt_rtcp"
	// ScenarioArchive is the end-to-end demo impairment: it combines
	// reordering, duplicates and a bounded burst loss the repairer must
	// record as a traceable gap, while leaving keyframes intact.
	ScenarioArchive Scenario = "archive"
)

// GroundTruth is the generator's exact accounting, used to score the
// pipeline metrics against the stream that was actually produced.
type GroundTruth struct {
	TotalPackets   int // RTP datagrams emitted before transport impairment
	Keyframes      int
	Lost           int // packets deliberately dropped
	Duplicates     int // extra duplicate datagrams injected
	Truncated      int // malformed/truncated RTP datagrams
	CorruptReports int // corrupted RTCP SRs
	ValidReports   int // well-formed RTCP SRs
	FirstSeq       []int64
	LastSeq        []int64
}

// Stream is a generated, reproducible transport capture.
type Stream struct {
	Datagrams [][]byte // wire order (impaired)
	Truth     GroundTruth
	Tracks    []TrackSpec
	Seed      int64
	Scenario  Scenario
}

// Config controls generation.
type Config struct {
	Seed      int64
	Tracks    []TrackSpec
	Scenario  Scenario
	SREvery   int // emit an RTCP SR every N packets per track (0 = none)
	BurstSize int // burst-loss length
}

// Generate builds a deterministic impaired stream from cfg.
func Generate(cfg Config) *Stream {
	rng := rand.New(rand.NewSource(cfg.Seed))
	s := &Stream{Tracks: cfg.Tracks, Seed: cfg.Seed, Scenario: cfg.Scenario}

	type item struct {
		raw  []byte
		hint modelPacketHint
		spec TrackSpec
	}
	var clean []item
	keyframes := 0
	srCount := map[uint32]int{}

	// 0. Emit an initial RTCP Sender Report per track BEFORE any packet so
	// the wall-clock anchor exists when the first packet is mapped. This
	// mirrors real RTP, where SRs establish timing up front and prevents a
	// late report from re-anchoring an already-running clock.
	if cfg.SREvery > 0 {
		for _, spec := range cfg.Tracks {
			ntp := ntpAt(0)
			sr := BuildSenderReport(spec.SSRC, 0, ntp, 0, 0)
			clean = append(clean, item{raw: sr})
			srCount[spec.SSRC]++
		}
	}

	// 1. Build clean per-track packets and interleave round-robin.
	per := maxPacketCount(cfg.Tracks)
	for ord := int64(0); ord < int64(per); ord++ {
		for _, spec := range cfg.Tracks {
			if int(ord) >= spec.Packets {
				continue
			}
			raw, hint, err := BuildPacket(spec, ord)
			if err != nil {
				continue
			}
			if hint.Keyframe {
				keyframes++
			}
			clean = append(clean, item{raw: raw, hint: hint, spec: spec})
			if cfg.SREvery > 0 && int(ord+1)%cfg.SREvery == 0 {
				srCount[spec.SSRC]++
				ntp := ntpAt(int64(hint.Timestamp) * 1_000_000_000 / int64(spec.ClockRate))
				sr := BuildSenderReport(spec.SSRC, hint.Timestamp, ntp,
					uint32(ord+1), uint32((ord+1))*64)
				clean = append(clean, item{raw: sr})
			}
		}
	}

	s.Truth.TotalPackets = countRTP(cfg.Tracks)
	s.Truth.Keyframes = keyframes
	s.Truth.ValidReports = len(srCount)

	// 2. Apply scenario-specific transport impairment in wire order.
	burst := cfg.BurstSize
	if burst <= 0 {
		burst = 7
	}
	drop := map[int]bool{}
	switch cfg.Scenario {
	case ScenarioBurstLoss, ScenarioArchive:
		// Drop a contiguous run of non-keyframe RTP packets. Preserving
		// keyframes keeps the keyframe index fully hit while still
		// producing a real, traceable loss gap (and no fabricated frames).
		start := len(clean) / 3
		for k := start; k < len(clean) && len(drop) < burst; k++ {
			if !isRTP(clean[k].raw) || clean[k].hint.Keyframe {
				continue
			}
			drop[k] = true
		}
		s.Truth.Lost = len(drop)
	case ScenarioTruncated:
		for k := len(clean) / 4; k < len(clean) && s.Truth.Truncated < 3; k += len(clean) / 3 {
			if isRTP(clean[k].raw) {
				cp := append([]byte(nil), clean[k].raw[:4]...)
				clean[k].raw = cp // truncated to a partial header
				s.Truth.Truncated++
			}
		}
	case ScenarioCorruptRTCP:
		for i := range clean {
			if !isRTP(clean[i].raw) && s.Truth.CorruptReports < 2 {
				clean[i].raw = corruptReport(clean[i].raw)
				s.Truth.CorruptReports++
				s.Truth.ValidReports--
			}
		}
	}

	// 3. Emit, applying reorder/duplicates and drops.
	for i, it := range clean {
		if drop[i] {
			continue
		}
		s.Datagrams = append(s.Datagrams, append([]byte(nil), it.raw...))
	}
	if cfg.Scenario == ScenarioReorderDup || cfg.Scenario == ScenarioArchive {
		impairReorderDup(rng, s)
		s.Truth.Duplicates = countDup(s)
	}
	s.recordSeqBounds()
	return s
}

// impairReorderDup swaps a bounded set of adjacent RTP datagrams and
// repeats a few, staying within what the reorder window can resolve.
func impairReorderDup(rng *rand.Rand, s *Stream) {
	n := len(s.Datagrams)
	swaps := n / 20
	for k := 0; k < swaps; k++ {
		i := 1 + rng.Intn(n-2)
		if isRTP(s.Datagrams[i]) && isRTP(s.Datagrams[i-1]) {
			s.Datagrams[i], s.Datagrams[i-1] = s.Datagrams[i-1], s.Datagrams[i]
		}
	}
	for k := 0; k < n/25; k++ {
		i := rng.Intn(n)
		if isRTP(s.Datagrams[i]) {
			dup := append([]byte(nil), s.Datagrams[i]...)
			s.Datagrams = append(s.Datagrams[:i+1], append([][]byte{dup}, s.Datagrams[i+1:]...)...)
			n++
		}
	}
}

func countDup(s *Stream) int {
	seen := map[[16]byte]int{}
	dups := 0
	for _, d := range s.Datagrams {
		if !isRTP(d) {
			continue
		}
		var key [16]byte
		copy(key[:], d[8:12]) // SSRC
		copy(key[4:6], d[2:4])
		seen[key]++
		if seen[key] > 1 {
			dups++
		}
	}
	return dups
}

func corruptReport(sr []byte) []byte {
	out := append([]byte(nil), sr...)
	// Zero the NTP field so the reader flags it as an invalid report.
	for i := 8; i < 16; i++ {
		out[i] = 0
	}
	return out
}

func (s *Stream) recordSeqBounds() {
	for _, t := range s.Tracks {
		s.Truth.FirstSeq = append(s.Truth.FirstSeq, 0)
		s.Truth.LastSeq = append(s.Truth.LastSeq, int64(t.Packets)-1)
	}
}

func countRTP(tracks []TrackSpec) int {
	n := 0
	for _, t := range tracks {
		n += t.Packets
	}
	return n
}

func maxPacketCount(tracks []TrackSpec) int {
	m := 0
	for _, t := range tracks {
		if t.Packets > m {
			m = t.Packets
		}
	}
	return m
}

func isRTP(raw []byte) bool {
	return len(raw) > 1 && raw[1] != 200
}
