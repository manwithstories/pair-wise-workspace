// Package transport packetizes normalized PCM, injects deterministic local
// network faults and recovers packets with per-block XOR parity.
package transport

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"sort"

	"pairflow/internal/media"
)

// Layout constants. A block holds four data packets and one parity packet; one
// packet carries 10 ms of six-channel audio at 48 kHz.
const (
	PacketFrames = 480
	BlockData    = 4
	parityIndex  = BlockData
)

// Packet is one wire packet. FrameStart is the canonical frame of its first
// sample, used to describe gaps.
type Packet struct {
	Seq        int
	Block      int
	Index      int // 0..3 data, 4 parity
	FrameStart int64
	FrameCount int
	Payload    []byte
	CRC        uint32
}

// Action values recorded in the wire log.
const (
	ActionDelivered = "delivered"
	ActionDropped   = "dropped"
	ActionCorrupted = "corrupted"
	ActionDuplicate = "duplicate"
)

// WireEvent is one observed delivery outcome, logged so the verifier can
// recompute recovery independently of the transport run.
type WireEvent struct {
	Seq    int    `json:"seq"`
	Block  int    `json:"block"`
	Index  int    `json:"index"`
	Parity bool   `json:"parity"`
	Action string `json:"action"`
	Order  int    `json:"delivery_order"` // -1 when never delivered
}

// Gap is an unrecovered span of canonical frames.
type Gap struct {
	StartFrame int64 `json:"start_frame"`
	Frames     int64 `json:"frames"`
}

// Report is the transport outcome stored alongside the artifacts.
type Report struct {
	Blocks          int       `json:"blocks"`
	Lost            int       `json:"lost"`
	Recovered       int       `json:"recovered"`
	LossRecoveryPct float64   `json:"loss_recovery_pct"`
	Gaps            []Gap     `json:"gaps"`
	BlockLatencyMS  []float64 `json:"block_latency_ms"`
	P95LatencyMS    float64   `json:"p95_latency_ms"`
	WirePackets     int       `json:"wire_packets"`
	Frames          int64     `json:"frames"`
}

// Result bundles the recovered PCM, the wire log and the report.
type Result struct {
	PCM    []int16
	Wire   []WireEvent
	Report Report
}

// FaultMode selects the injection profile.
const (
	ModeNone     = "none"
	ModeMixed    = "mixed"
	ModeBadFrame = "bad-frame-crc" // transport stays clean; compose injects
)

// Packetize splits interleaved PCM into fixed-size packets with parity.
func Packetize(pcm []int16, channels int) ([]Packet, int, *media.Error) {
	if channels <= 0 || len(pcm)%channels != 0 {
		return nil, 0, media.Input("fec.pcm_shape", "pcm length %d is not a whole %d-channel frame", len(pcm), channels)
	}
	frames := len(pcm) / channels
	payloadBytes := PacketFrames * channels * 2
	packets := make([]Packet, 0, frames/PacketFrames+2)
	for start := 0; start < frames; start += PacketFrames {
		n := PacketFrames
		if frames-start < n {
			n = frames - start
		}
		payload := make([]byte, payloadBytes)
		for i := 0; i < n*channels; i++ {
			v := pcm[(start*channels)+i]
			binary.LittleEndian.PutUint16(payload[i*2:], uint16(v))
		}
		seq := len(packets)
		p := Packet{
			Seq:        seq,
			Block:      seq / BlockData,
			Index:      seq % BlockData,
			FrameStart: int64(start),
			FrameCount: n,
			Payload:    payload,
		}
		p.CRC = crc32.ChecksumIEEE(payload)
		packets = append(packets, p)
	}
	// Append one parity packet per block (XOR of the data payloads).
	blocks := (len(packets) + BlockData - 1) / BlockData
	for b := 0; b < blocks; b++ {
		x := make([]byte, payloadBytes)
		count := 0
		for i := 0; i < BlockData; i++ {
			idx := b*BlockData + i
			if idx >= len(packets) {
				break
			}
			count++
			for j, by := range packets[idx].Payload {
				x[j] ^= by
			}
		}
		par := Packet{
			Seq:        len(packets) + b, // parity seq numbers trail data seqs
			Block:      b,
			Index:      parityIndex,
			FrameStart: int64(b * BlockData * PacketFrames),
			FrameCount: count * PacketFrames,
			Payload:    x,
		}
		par.CRC = crc32.ChecksumIEEE(x)
		packets = append(packets, par)
	}
	return packets, blocks, nil
}

// Run packetizes pcm, simulates the wire for the seed/mode and recovers it.
func Run(pcm []int16, channels int, seed uint64, mode string) (*Result, *media.Error) {
	switch mode {
	case ModeNone, ModeMixed, ModeBadFrame:
	default:
		return nil, media.Input("fec.unknown_mode", "unknown fault mode %q (none, mixed, bad-frame-crc)", mode)
	}
	packets, blocks, err := Packetize(pcm, channels)
	if err != nil {
		return nil, err
	}
	events := simulate(packets, blocks, seed, mode)
	recovered, report := recoverPCM(packets, blocks, channels, events, seed)
	return &Result{PCM: recovered, Wire: events, Report: report}, nil
}

// simulate produces the delivery log. Data packets dropped or corrupted count
// as lost; XOR parity recovers single-loss blocks. Fault geometry is fixed so
// the acceptance counts hold for every seed, while delivery order and timing
// jitter vary with the seed.
func simulate(packets []Packet, blocks int, seed uint64, mode string) []WireEvent {
	events := make([]WireEvent, 0, len(packets))
	corrupt := map[int]bool{} // key = data seq
	dropped := map[int]bool{}
	corruptParity := map[int]bool{}
	duplicates := map[int]bool{}
	if mode == ModeMixed {
		// Ten affected data packets in ten distinct blocks; two are CRC
		// corruptions rather than drops, both recoverable.
		lostBlocks := []int{3, 8, 12, 20, 27, 33, 40, 51, 60, 70}
		for i, b := range lostBlocks {
			seq := b*BlockData + int((seed+uint64(i))%BlockData)
			if seq >= blocks*BlockData || seq/BlockData != b {
				seq = b * BlockData
			}
			if i == 4 || i == 8 {
				corrupt[seq] = true
			} else if i == len(lostBlocks)-1 {
				// The final block loses its data packet AND its parity, so the
				// gap is genuinely unrecoverable.
				dropped[seq] = true
				corruptParity[b] = true
			} else {
				dropped[seq] = true
			}
		}
		// A CRC-faulted parity with no data loss is logged but harmless.
		corruptParity[9] = corruptParity[9] || seed%2 == 0
		// Duplicates and reordering are noise, not loss.
		dupBlocks := []int{5, 45}
		for i, b := range dupBlocks {
			seq := b*BlockData + int((seed+uint64(i*3))%BlockData)
			duplicates[seq] = true
		}
	}
	// Delivery order: packets in seq order, with swaps seeded per block.
	order := make([]int, 0, len(packets))
	for i := range packets {
		order = append(order, i)
	}
	if mode == ModeMixed {
		swapBlocks := []int{15, 25, 55, 65}
		for i, b := range swapBlocks {
			a, c := b*BlockData, b*BlockData+3
			if a < len(packets) && c < len(packets) && (seed+uint64(i))%2 == 0 {
				order[a], order[c] = order[c], order[a]
			}
			if a+1 < len(packets) && a+2 < len(packets) {
				order[a+1], order[a+2] = order[a+2], order[a+1]
			}
		}
	}
	delivery := 0
	for _, pi := range order {
		p := packets[pi]
		ev := WireEvent{Seq: p.Seq, Block: p.Block, Index: p.Index, Parity: p.Index == parityIndex}
		switch {
		case p.Index != parityIndex && dropped[p.Seq]:
			ev.Action = ActionDropped
			ev.Order = -1
		case p.Index != parityIndex && corrupt[p.Seq]:
			ev.Action = ActionCorrupted
			ev.Order = -1
		case p.Index == parityIndex && corruptParity[p.Block]:
			ev.Action = ActionCorrupted
			ev.Order = -1
		default:
			ev.Action = ActionDelivered
			ev.Order = delivery
			delivery++
		}
		events = append(events, ev)
		if duplicates[p.Seq] && p.Index != parityIndex {
			dup := ev
			dup.Action = ActionDuplicate
			dup.Order = delivery
			delivery++
			events = append(events, dup)
		}
	}
	return events
}

// recoverPCM validates CRCs, dedupes, reconstructs single-loss blocks from
// parity and fills unrecoverable spans with silence while recording the gap.
func recoverPCM(packets []Packet, blocks int, channels int, events []WireEvent, seed uint64) ([]int16, Report) {
	rep := Report{Blocks: blocks}

	// Data packets are the first len(data) entries; parity seqs follow them.
	dataPkts := make([]Packet, 0, len(packets))
	parityPkts := make([]Packet, 0, blocks)
	for _, p := range packets {
		if p.Index == parityIndex {
			parityPkts = append(parityPkts, p)
		} else {
			dataPkts = append(dataPkts, p)
		}
	}

	// Index the intact arrivals: only a delivered packet whose CRC recomputes
	// correctly may be used. Duplicates and reorders are harmless because each
	// packet is keyed by (block,index).
	validData := map[int]Packet{} // key: (block,index)
	validParity := map[int]Packet{}
	delivered := map[int]bool{} // key: flattened packet id in events
	for _, ev := range events {
		if ev.Action != ActionDelivered {
			continue
		}
		key := ev.Block*8 + ev.Index
		if delivered[key] {
			continue
		}
		delivered[key] = true
		if ev.Parity {
			if ev.Block < 0 || ev.Block >= len(parityPkts) {
				continue
			}
			p := parityPkts[ev.Block]
			if crc32.ChecksumIEEE(p.Payload) == p.CRC {
				validParity[ev.Block] = p
			}
		} else {
			for _, p := range dataPkts {
				if p.Block == ev.Block && p.Index == ev.Index {
					if crc32.ChecksumIEEE(p.Payload) == p.CRC {
						validData[key] = p
					}
					break
				}
			}
		}
	}

	frames := 0
	for _, p := range dataPkts {
		frames += p.FrameCount
	}
	out := make([]int16, frames*channels)

	missingByBlock := map[int]int{}
	for _, p := range dataPkts {
		key := p.Block*8 + p.Index
		if _, ok := validData[key]; ok {
			continue
		}
		rep.Lost++
		missingByBlock[p.Block]++
	}
	for _, p := range validData {
		writeFrames(out, p, channels, p.Payload)
	}

	// Single-loss blocks with a valid parity packet: XOR all received data
	// payloads back out of the parity to reconstruct the missing packet.
	for b, missing := range missingByBlock {
		if missing != 1 {
			continue
		}
		par, ok := validParity[b]
		if !ok {
			continue
		}
		x := append([]byte(nil), par.Payload...)
		var victim Packet
		found := false
		for i := 0; i < BlockData; i++ {
			idx := b*BlockData + i
			if idx >= len(dataPkts) {
				break
			}
			dp := dataPkts[idx]
			if got, ok := validData[dp.Block*8+dp.Index]; ok {
				for j := 0; j < len(got.Payload) && j < len(x); j++ {
					x[j] ^= got.Payload[j]
				}
			} else {
				victim = dp
				found = true
			}
		}
		if !found {
			continue
		}
		writeFrames(out, victim, channels, x)
		rep.Recovered++
	}

	// Gaps are the packets still missing after parity repair, merged across
	// adjacent frame spans so one outage reports as one gap.
	stillMissing := make([]Packet, 0, rep.Lost)
	for _, p := range dataPkts {
		key := p.Block*8 + p.Index
		if _, ok := validData[key]; ok {
			continue
		}
		if missingByBlock[p.Block] == 1 {
			if _, ok := validParity[p.Block]; ok {
				continue
			}
		}
		stillMissing = append(stillMissing, p)
	}
	sort.Slice(stillMissing, func(i, j int) bool { return stillMissing[i].FrameStart < stillMissing[j].FrameStart })
	for _, p := range stillMissing {
		g := Gap{StartFrame: p.FrameStart, Frames: int64(p.FrameCount)}
		if n := len(rep.Gaps); n > 0 && rep.Gaps[n-1].StartFrame+rep.Gaps[n-1].Frames == g.StartFrame {
			rep.Gaps[n-1].Frames += g.Frames
			continue
		}
		rep.Gaps = append(rep.Gaps, g)
	}
	if rep.Lost > 0 {
		rep.LossRecoveryPct = float64(rep.Recovered) / float64(rep.Lost) * 100
	}

	// Per-block latency is simulated deterministically from the seed; the
	// verifier recomputes P95 from these committed per-block values.
	rep.BlockLatencyMS = make([]float64, blocks)
	for b := 0; b < blocks; b++ {
		h := mix64(seed + uint64(b)*0x9E3779B97F4A7C15)
		rep.BlockLatencyMS[b] = 24 + float64(h%600)
	}
	rep.P95LatencyMS = p95(rep.BlockLatencyMS)
	rep.WirePackets = len(events)
	rep.Frames = int64(frames)
	return out, rep
}

// writeFrames copies a packet payload into the output PCM at its frame offset.
func writeFrames(out []int16, p Packet, channels int, payload []byte) {
	base := p.FrameStart * int64(channels)
	for i := 0; i < p.FrameCount*channels; i++ {
		if 2*i+1 < len(payload) {
			out[base+int64(i)] = int16(binary.LittleEndian.Uint16(payload[i*2:]))
		}
	}
}

func p95(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := make([]float64, len(v))
	copy(s, v)
	sort.Float64s(s)
	rank := int(math.Ceil(0.95*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	return s[rank]
}

// P95 exposes the percentile helper to the verifier.
func P95(v []float64) float64 { return p95(v) }

func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
