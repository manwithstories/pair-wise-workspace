// Package fixture generates all local, deterministic test scenarios. No
// network or device access happens: scenarios are materialized as RTP packet
// byte slices plus, where relevant, a malformed WAV byte slice.
package fixture

import (
	"encoding/binary"
	"math/rand"
	"sort"

	"github.com/pion/rtp"

	"avcheck/internal/clock"
	"avcheck/internal/ingest"
	"avcheck/internal/wav"
)

// Scenario identifies one of the six required fixture classes.
type Scenario string

const (
	ScenarioNormal        Scenario = "normal"
	ScenarioReorderDup    Scenario = "reorder-duplicate"
	ScenarioLossBurst     Scenario = "loss-burst"
	ScenarioLateOverflow  Scenario = "late-overflow"
	ScenarioCorruptWAV    Scenario = "corrupt-wav"
	ScenarioQueueBoundary Scenario = "queue-boundary"
)

// Config controls fixture generation.
type Config struct {
	Seed         int64
	DurationMS   int
	Tracks       int
	Channels     int
	FramesPerPkt int
	LossBurst    int // max consecutive packets dropped per track
	Reorder      int // reorder spread in packet positions
	Duplicate    int // number of duplicated packets
}

// DefaultConfig returns the values used by the acceptance command.
func DefaultConfig() Config {
	return Config{
		Seed:         7,
		DurationMS:   10000,
		Tracks:       3,
		Channels:     2,
		FramesPerPkt: int(clock.TicksPerFrame()) / 4, // four packets per 30 fps frame
	}
}

// Packet is one generated RTP datagram with its virtual arrival time.
type Packet struct {
	TrackID   uint32
	SSRC      uint32
	Raw       []byte
	ArrivalMS int64
	Seq       uint16
}

// Generator produces deterministic multi-track packet streams.
type Generator struct {
	cfg   Config
	rng   *rand.Rand
	pkts  map[uint32][]*rtp.Packet
	times map[uint32][]int64
}

// NewGenerator seeds a generator.
func NewGenerator(cfg Config) *Generator {
	return &Generator{
		cfg:   cfg,
		rng:   rand.New(rand.NewSource(cfg.Seed)),
		pkts:  make(map[uint32][]*rtp.Packet),
		times: make(map[uint32][]int64),
	}
}

// packetsPerTrack derives the total packet count from duration.
func (g *Generator) packetsPerTrack() int {
	return g.cfg.SampleCount() / g.cfg.FramesPerPkt
}

// SampleCount returns the total PCM frames per track for the duration.
func (c Config) SampleCount() int {
	return clock.SampleRate * c.DurationMS / 1000
}

// BuildTracks synthesizes clean per-track RTP packets with PCM payloads.
func (g *Generator) BuildTracks() {
	count := g.packetsPerTrack()
	for t := 0; t < g.cfg.Tracks; t++ {
		track := uint32(t + 1)
		ssrc := uint32(0x1000 + t)
		pz := ingest.NewPacketizer(ssrc, track, g.cfg.Channels, g.cfg.FramesPerPkt)
		list := make([]*rtp.Packet, 0, count)
		arrivals := make([]int64, 0, count)
		for i := 0; i < count; i++ {
			frames := make([]int32, g.cfg.FramesPerPkt*g.cfg.Channels)
			for f := range frames {
				// Deterministic tone-like waveform, distinct per track.
				phase := (i*g.cfg.FramesPerPkt + f) * (t + 1)
				frames[f] = int32((phase*97)%20000 - 10000)
			}
			pkt, _ := pz.Packetize(frames)
			list = append(list, pkt)
			arrivals = append(arrivals, int64(i)*g.packetSpacingMS())
		}
		g.pkts[track] = list
		g.times[track] = arrivals
	}
}

func (g *Generator) packetSpacingMS() int64 {
	return int64(g.cfg.FramesPerPkt) * 1000 / int64(clock.SampleRate)
}

// Normal returns packets in arrival order, untouched.
func (g *Generator) Normal() []Packet {
	return g.materialize(g.orderedIndices())
}

// ReorderAndDuplicate swaps nearby packets and re-injects duplicates.
func (g *Generator) ReorderAndDuplicate() []Packet {
	idx := g.orderedIndices()
	spread := g.cfg.Reorder
	if spread <= 0 {
		spread = 4
	}
	for _, t := range g.sortedTracks() {
		list := idx[t]
		for swap := 0; swap < spread && len(list) > 2; swap++ {
			i := 1 + g.rng.Intn(len(list)-1)
			j := i - 1
			list[i], list[j] = list[j], list[i]
		}
	}
	dups := g.cfg.Duplicate
	out := g.materialize(idx)
	for d := 0; d < dups && len(out) > 0; d++ {
		p := out[g.rng.Intn(len(out))]
		cp := append([]byte(nil), p.Raw...)
		out = append(out, Packet{
			TrackID: p.TrackID, SSRC: p.SSRC, Raw: cp,
			ArrivalMS: p.ArrivalMS + 1, Seq: p.Seq,
		})
	}
	return out
}

// LossBurst removes runs of consecutive packets up to cfg.LossBurst. It also
// reports the per-track dropped sequence numbers for test assertions.
func (g *Generator) LossBurst() ([]Packet, map[uint32][]uint16) {
	idx := g.orderedIndices()
	dropped := make(map[uint32][]uint16)
	burst := g.cfg.LossBurst
	if burst <= 0 {
		burst = 3
	}
	for _, track := range g.sortedTracks() {
		list := idx[track]
		if len(list) < burst*3 {
			continue
		}
		start := 5 + g.rng.Intn(len(list)-burst*2)
		for k := 0; k < burst; k++ {
			dropped[track] = append(dropped[track], g.pkts[track][list[start+k]].SequenceNumber)
		}
		list = append(list[:start], list[start+burst:]...)
		idx[track] = list
	}
	return g.materialize(idx), dropped
}

// LateOverflow holds packets beyond the jitter window horizon and releases
// them late, so the window classifies them as overflow/late.
func (g *Generator) LateOverflow(windowPackets int) []Packet {
	idx := g.orderedIndices()
	var held []Packet
	out := make([]Packet, 0)
	for _, track := range g.sortedTracks() {
		list := idx[track]
		if len(list) <= windowPackets+2 {
			out = append(out, g.trackPackets(track, list)...)
			continue
		}
		latePos := windowPackets + 1
		lateIdx := list[latePos]
		early := append(append([]int(nil), list[:latePos]...), list[latePos+1:]...)
		out = append(out, g.trackPackets(track, early)...)
		pkt := g.pkts[track][lateIdx]
		raw, _ := pkt.Marshal()
		held = append(held, Packet{
			TrackID: track, SSRC: pkt.SSRC, Raw: raw,
			ArrivalMS: g.times[track][lateIdx] + int64(windowPackets)*g.packetSpacingMS()*4,
			Seq:       pkt.SequenceNumber,
		})
	}
	return append(out, held...)
}

// CorruptWAV returns a truncated WAV byte stream and the byte offset where the
// truncation begins.
// CorruptWAV returns a WAV stream whose data payload is truncated. The RIFF
// size is rewritten to match the shortened container so the parser walks the
// full header and reports the exact byte offset where the data ends early.
func CorruptWAV() ([]byte, int64) {
	p := &wav.PCM{
		Channels:    2,
		BitsPerSamp: 16,
		SampleRate:  clock.SampleRate,
		Samples:     make([]int32, 64),
	}
	full := wav.Encode(p, 16)
	cut := int64(len(full)) - 20 // payload ends 20 bytes early
	out := append([]byte(nil), full[:cut]...)
	// RIFF size field (offset 4) = total bytes minus the 8-byte RIFF header.
	binary.LittleEndian.PutUint32(out[4:8], uint32(cut-8))
	return out, cut
}

func (g *Generator) orderedIndices() map[uint32][]int {
	idx := make(map[uint32][]int, len(g.pkts))
	for _, track := range g.sortedTracks() {
		list := g.pkts[track]
		ns := make([]int, len(list))
		for i := range list {
			ns[i] = i
		}
		idx[track] = ns
	}
	return idx
}

// sortedTracks returns track IDs in ascending order so RNG consumption and
// output flattening stay deterministic across runs.
func (g *Generator) sortedTracks() []uint32 {
	tracks := make([]uint32, 0, len(g.pkts))
	for track := range g.pkts {
		tracks = append(tracks, track)
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i] < tracks[j] })
	return tracks
}

func (g *Generator) trackPackets(track uint32, indices []int) []Packet {
	out := make([]Packet, 0, len(indices))
	for _, i := range indices {
		pkt := g.pkts[track][i]
		raw, _ := pkt.Marshal()
		out = append(out, Packet{
			TrackID: track, SSRC: pkt.SSRC, Raw: raw,
			ArrivalMS: g.times[track][i], Seq: pkt.SequenceNumber,
		})
	}
	return out
}

// materialize flattens per-track index maps into one packet slice, sorted by
// virtual arrival time while preserving per-track ordering.
func (g *Generator) materialize(idx map[uint32][]int) []Packet {
	var all []Packet
	for _, track := range g.sortedTracks() {
		all = append(all, g.trackPackets(track, idx[track])...)
	}
	// Stable sort keeps same-track packet order on arrival ties.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].ArrivalMS < all[j-1].ArrivalMS; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	return all
}
