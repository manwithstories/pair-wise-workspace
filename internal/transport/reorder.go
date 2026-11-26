package transport

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"mediapipe/internal/media"
)

// Config controls the deterministic weak-network simulator.
type Config struct {
	Seed                   int64 `json:"seed"`
	DuplicateCount         int   `json:"duplicate_packets"`
	ReorderBursts          int   `json:"reorder_bursts"`
	RecoverableLossCount   int   `json:"recoverable_losses"`
	UnrecoverableLossCount int   `json:"unrecoverable_losses"`
	BandwidthBaseKbps      int   `json:"bandwidth_base_kbps"`
}

func DefaultConfig(seed int64, packetCount int) Config {
	if packetCount < 8 {
		packetCount = 8
	}
	scale := packetCount / 24
	if scale < 1 {
		scale = 1
	}
	return Config{
		Seed:                   seed,
		DuplicateCount:         2 * scale,
		ReorderBursts:          3 * scale,
		RecoverableLossCount:   2 * scale,
		UnrecoverableLossCount: 0,
		BandwidthBaseKbps:      8_000,
	}
}

type Stats struct {
	Received             int   `json:"received_packets"`
	Delivered            int   `json:"delivered_packets"`
	DuplicatePackets     int   `json:"duplicate_packets"`
	ReorderedPackets     int   `json:"reordered_packets"`
	RecoveredPackets     int   `json:"recovered_packets"`
	LostPackets          int   `json:"lost_packets"`
	UnrecoverablePackets int   `json:"unrecoverable_packets"`
	AvgBandwidthKbps     int   `json:"avg_bandwidth_kbps,omitempty"`
	BandwidthTrace       []int `json:"-"`
}

// Simulator produces a repeatable impaired packet arrival sequence.
type Simulator struct {
	cfg           Config
	rng           *rand.Rand
	duplicates    map[uint64]int
	recoverable   map[uint64]bool
	unrecoverable map[uint64]bool
	swapped       map[uint64]bool
	Bandwidth     []int
	Stats         Stats
}

func NewSimulator(cfg Config, packetCount int) *Simulator {
	if cfg.BandwidthBaseKbps <= 0 {
		cfg.BandwidthBaseKbps = 8_000
	}
	s := &Simulator{
		cfg:           cfg,
		rng:           rand.New(rand.NewSource(cfg.Seed)),
		duplicates:    map[uint64]int{},
		recoverable:   map[uint64]bool{},
		unrecoverable: map[uint64]bool{},
		swapped:       map[uint64]bool{},
		Bandwidth:     make([]int, packetCount),
	}
	chooseBursts(s.rng, packetCount, cfg.RecoverableLossCount, s.recoverable)
	chooseBursts(s.rng, packetCount, cfg.UnrecoverableLossCount, s.unrecoverable)
	for len(s.duplicates) < cfg.DuplicateCount && len(s.duplicates) < packetCount/3 {
		seq := uint64(2 + s.rng.Intn(max(1, packetCount-2)))
		s.duplicates[seq] = 1 + s.rng.Intn(2)
	}
	for i := 0; i < packetCount; i++ {
		wave := 0.22 * math.Sin(float64(i)/2.7)
		burst := 0.0
		if i%17 < 3 {
			burst = -0.45
		}
		jitter := (s.rng.Float64() - 0.5) * 0.16
		kbps := float64(cfg.BandwidthBaseKbps) * (1 + wave + burst + jitter)
		if kbps < 300 {
			kbps = 300
		}
		s.Bandwidth[i] = int(kbps)
	}
	if packetCount > 0 {
		s.Stats.AvgBandwidthKbps = averageInts(s.Bandwidth)
	}
	return s
}

// Transmit returns packets in arrival order. Lost packets are omitted until a
// recovery pass explicitly retrieves them.
func (s *Simulator) Transmit(in []media.Packet) []media.Packet {
	n := len(in)
	order := make([]uint64, n)
	bySeq := make(map[uint64]media.Packet, n)
	for i, p := range in {
		seq := p.Sequence
		if seq == 0 {
			seq = uint64(i + 1)
			in[i].Sequence = seq
		}
		order[i] = seq
		bySeq[seq] = in[i]
	}
	for b := 0; b < s.cfg.ReorderBursts && n > 4; b++ {
		at := 1 + s.rng.Intn(n-3)
		width := 1 + s.rng.Intn(3)
		for k := 0; k < width && at+1 < n; k++ {
			order[at], order[at+1] = order[at+1], order[at]
			s.swapped[order[at]] = true
			s.swapped[order[at+1]] = true
			at++
		}
	}
	out := make([]media.Packet, 0, n+len(s.duplicates))
	for _, seq := range order {
		if s.recoverable[seq] || s.unrecoverable[seq] {
			continue
		}
		p := bySeq[seq]
		out = append(out, p)
		for d := 0; d < s.duplicates[seq]; d++ {
			dup := p
			dup.Payload = append([]byte(nil), p.Payload...)
			out = append(out, dup)
			s.Stats.DuplicatePackets++
		}
	}
	s.Stats.ReorderedPackets = len(s.swapped)
	return out
}

func (s *Simulator) IsUnrecoverable(seq uint64) bool { return s.unrecoverable[seq] }
func (s *Simulator) RecoverableMissing() []uint64    { return sortedKeys(s.recoverable) }
func (s *Simulator) UnrecoverableMissing() []uint64  { return sortedKeys(s.unrecoverable) }

// Retriever reloads a checkpointed sequence from the supplier/demuxer.
type Retriever func(seq uint64) (media.Packet, bool, error)

// ReorderBuffer deduplicates and restores sequence order.
type ReorderBuffer struct {
	next     uint64
	maxSeen  uint64
	buffered map[uint64]media.Packet
	seen     map[uint64]bool
	Stats    Stats
}

func NewReorderBuffer(next uint64) *ReorderBuffer {
	if next == 0 {
		next = 1
	}
	return &ReorderBuffer{next: next, buffered: map[uint64]media.Packet{}, seen: map[uint64]bool{}}
}

func (rb *ReorderBuffer) Checkpoint() uint64 {
	if rb.next == 0 {
		return 0
	}
	return rb.next - 1
}

// Accept ingests one arrival and returns any newly contiguous packets.
func (rb *ReorderBuffer) Accept(p media.Packet) []media.Packet {
	rb.Stats.Received++
	seq := p.Sequence
	if seq < rb.next || rb.seen[seq] {
		rb.Stats.DuplicatePackets++
		return nil
	}
	rb.seen[seq] = true
	if seq > rb.maxSeen {
		rb.maxSeen = seq
	}
	if seq > rb.next {
		rb.buffered[seq] = p
		rb.Stats.ReorderedPackets++
		return nil
	}
	delete(rb.buffered, seq)
	out := []media.Packet{p}
	if p.Recovered {
		rb.Stats.RecoveredPackets++
	}
	rb.next++
	for {
		q, ok := rb.buffered[rb.next]
		if !ok {
			break
		}
		delete(rb.buffered, rb.next)
		if q.Recovered {
			rb.Stats.RecoveredPackets++
		}
		out = append(out, q)
		rb.next++
	}
	rb.Stats.Delivered += len(out)
	return out
}

// Missing returns gaps from the checkpoint through throughSeq.
func (rb *ReorderBuffer) Missing(throughSeq uint64) []uint64 {
	var miss []uint64
	for seq := rb.next; seq <= throughSeq; seq++ {
		if rb.seen[seq] {
			continue
		}
		miss = append(miss, seq)
	}
	return miss
}

// Recover resumes after checkpoint and retrieves missing sequences.
func (rb *ReorderBuffer) Recover(throughSeq, checkpoint uint64, retrieve Retriever) ([]media.Packet, error) {
	if checkpoint+1 > rb.next {
		rb.next = checkpoint + 1
	}
	var recovered []media.Packet
	for _, seq := range rb.Missing(throughSeq) {
		if seq <= checkpoint {
			continue
		}
		p, ok, err := retrieve(seq)
		if err != nil {
			return recovered, media.WrapError(media.CodeIO, "retrieve packet", err)
		}
		if !ok {
			rb.Stats.UnrecoverablePackets++
			return recovered, media.NewError(media.CodeUnrecoverableLoss,
				fmt.Sprintf("sequence %d is unavailable after checkpoint %d", seq, checkpoint), nil)
		}
		p.Sequence = seq
		p.Recovered = true
		recovered = append(recovered, rb.Accept(p)...)
	}
	return recovered, nil
}

func averageInts(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return sum / len(xs)
}

func chooseBursts(rng *rand.Rand, packetCount, total int, out map[uint64]bool) {
	remaining := total
	for remaining > 0 && len(out) < packetCount/3 {
		length := remaining
		if length >= 2 {
			length = 2 + rng.Intn(3) // bursts of 2..4 consecutive sequence numbers
			if length > remaining {
				length = remaining
			}
		}
		start := 2 + rng.Intn(max(1, packetCount-length-2))
		added := 0
		for k := 0; k < length; k++ {
			seq := uint64(start + k)
			if int(seq) < packetCount && !out[seq] {
				out[seq] = true
				added++
				remaining--
			}
		}
		if added == 0 {
			continue
		}
	}
}

func sortedKeys(m map[uint64]bool) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
