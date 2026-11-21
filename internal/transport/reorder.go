// Package transport ingests RTP packet streams: it extends 16-bit sequence
// numbers across wrap, removes duplicates, reorders within a bounded window,
// turns burst loss into explicit gaps, and adapts the target bitrate from a
// sliding window of samples.
package transport

import (
	"fmt"
	"math"
	"sort"

	"github.com/pion/rtp"

	"mediad/internal/model"
)

// Packet is one ingested RTP packet with its extended sequence number. The
// extension is monotonic across 16-bit wrap, so ordering is stable.
type Packet struct {
	Seq         uint32 // original 16-bit sequence number
	ExtSeq      int64  // unwrapped, monotonic sequence number
	SSRC        uint32
	PayloadType uint8
	Marker      bool
	Timestamp   uint32
	Size        int
	Arrival     int64 // arrival order (input index)
	Payload     []byte
}

// Gap is an explicit, contiguous run of missing packets.
type Gap struct {
	StartExt int64 // first missing extended sequence
	Count    int64
	// Blocked reports whether this gap reached the burst threshold and
	// therefore blocks dependent frames.
	Blocked bool
	// From and To are the RTP timestamps of the packets bracketing the gap.
	// A zero bracket means the gap starts the stream or ends it.
	From, To uint32
}

// LastExt returns the last missing sequence (inclusive).
func (g Gap) LastExt() int64 { return g.StartExt + g.Count - 1 }

// StreamStats is the transport outcome used by the manifest and CLI.
type StreamStats struct {
	Received     int
	Unique       int
	Duplicates   int
	Reordered    int
	Lost         int
	Late         int
	Wrapped      bool
	CommittedExt int64
	FirstExt     int64
	LastExt      int64
	Gaps         []Gap
	LatePackets  []int64
}

// BlockedGaps returns only the gaps that reached the burst threshold.
func (s StreamStats) BlockedGaps() []Gap {
	var out []Gap
	for _, g := range s.Gaps {
		if g.Blocked {
			out = append(out, g)
		}
	}
	return out
}

// ReorderConfig configures the reorder buffer.
type ReorderConfig struct {
	// Window is how many sequences past the next expected packet may be
	// buffered before the hole is frozen into a gap.
	Window int
	// StartupHold is how many packets are buffered before the stream's start
	// point is fixed. It prevents an initially out-of-order packet from being
	// mistaken for a late one.
	StartupHold int
	// BurstGapThreshold is the missing-packet run length that blocks dependent
	// frames. Shorter gaps are recorded but remain playable.
	BurstGapThreshold int
}

// DefaultReorderConfig returns production defaults.
func DefaultReorderConfig() ReorderConfig {
	return ReorderConfig{Window: 64, StartupHold: 8, BurstGapThreshold: 4}
}

// Reorderer removes duplicates and orders packets across wrap.
//
// Contract:
//   - packets are delivered in strictly increasing extended sequence order;
//   - a sequence is delivered at most once;
//   - once a gap has been frozen (window overflow or flush), any later packet
//     for it is refused as late and can never overwrite committed output.
type Reorderer struct {
	cfg ReorderConfig

	started  bool
	anchor   int64 // highest extended sequence observed
	wrapped  bool
	firstExt int64
	hasFirst bool
	maxSeen  int64

	nextExpected  int64 // next sequence to deliver; -1 before startup ends
	buf           map[int64]Packet
	seen          map[int64]struct{}
	out           []Packet
	gaps          []Gap
	late          []int64
	dups          int
	reord         int
	lateN         int
	lastDelivered Packet
	haveLast      bool
}

// NewReorderer creates a reorderer.
func NewReorderer(cfg ReorderConfig) *Reorderer {
	if cfg.Window <= 0 {
		cfg.Window = 64
	}
	if cfg.StartupHold <= 0 {
		cfg.StartupHold = 8
	}
	if cfg.BurstGapThreshold <= 0 {
		cfg.BurstGapThreshold = 4
	}
	return &Reorderer{
		cfg: cfg, buf: make(map[int64]Packet), seen: make(map[int64]struct{}),
		anchor: -1, maxSeen: -1, nextExpected: -1,
	}
}

// Extend converts a 16-bit sequence into a monotonic extension.
func (r *Reorderer) Extend(seq uint16) int64 {
	s := int64(seq)
	if !r.started {
		r.started = true
		r.anchor = s
		return s
	}
	base := r.anchor &^ 0xFFFF
	cand := base | s
	const half = 0x8000
	switch {
	case cand < r.anchor-half:
		// Jumped forward past wrap, e.g. 65534 -> 2.
		r.wrapped = true
		return base + 0x10000 + s
	case cand > r.anchor+half:
		// Belongs to the previous epoch.
		return base - 0x10000 + s
	}
	return cand
}

// Push ingests one marshalled RTP packet.
func (r *Reorderer) Push(raw []byte, arrival int64) (Packet, bool, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		return Packet{}, false, model.Wrap(model.CategoryTransport, "rtp-unmarshal", err).
			WithBlockingDep("transport")
	}
	return r.PushPacket(pkt, arrival)
}

// PushPacket ingests an already-decoded RTP packet and reports whether it was
// accepted. Duplicates and late packets are counted but not accepted.
func (r *Reorderer) PushPacket(pkt rtp.Packet, arrival int64) (Packet, bool, error) {
	ext := r.Extend(pkt.SequenceNumber)
	packet := Packet{
		Seq: uint32(pkt.SequenceNumber), ExtSeq: ext, SSRC: pkt.SSRC,
		PayloadType: pkt.PayloadType, Marker: pkt.Marker,
		Timestamp: pkt.Timestamp, Size: len(pkt.Payload), Arrival: arrival,
		Payload: append([]byte(nil), pkt.Payload...),
	}
	if _, dup := r.seen[ext]; dup {
		r.dups++
		return packet, false, nil
	}
	if r.nextExpected >= 0 && ext < r.nextExpected {
		// The interval was already delivered or frozen into a gap. Drop it so
		// a late packet can never overwrite committed output.
		r.lateN++
		r.late = append(r.late, ext)
		return packet, false, nil
	}
	if r.maxSeen >= 0 && ext < r.maxSeen {
		r.reord++
	}
	r.seen[ext] = struct{}{}
	if ext > r.anchor {
		r.anchor = ext
	}
	if ext > r.maxSeen {
		r.maxSeen = ext
	}
	if !r.hasFirst || ext < r.firstExt {
		r.firstExt = ext
		r.hasFirst = true
	}
	r.buf[ext] = packet
	if r.nextExpected < 0 {
		if len(r.seen) < r.cfg.StartupHold {
			return packet, true, nil
		}
		r.nextExpected = r.firstExt
	}
	r.deliverContiguous()
	if len(r.buf) > r.cfg.Window {
		r.freezeSmallestHole()
	}
	return packet, true, nil
}

// deliverContiguous delivers every buffered packet at nextExpected.
func (r *Reorderer) deliverContiguous() {
	for {
		p, ok := r.buf[r.nextExpected]
		if !ok {
			return
		}
		delete(r.buf, r.nextExpected)
		r.out = append(r.out, p)
		r.lastDelivered = p
		r.haveLast = true
		r.nextExpected++
	}
}

// freezeSmallestHole turns the smallest pending hole into an explicit gap and
// delivers whatever became contiguous, keeping the buffer bounded.
func (r *Reorderer) freezeSmallestHole() {
	if len(r.buf) == 0 {
		return
	}
	pending := make([]int64, 0, len(r.buf))
	for ext := range r.buf {
		pending = append(pending, ext)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i] < pending[j] })
	target := pending[0]
	if target <= r.nextExpected {
		r.deliverContiguous()
		return
	}
	var from uint32
	if r.haveLast {
		from = r.lastDelivered.Timestamp
	}
	r.recordGap(r.nextExpected, target-1, from, r.buf[target].Timestamp)
	r.nextExpected = target
	r.deliverContiguous()
}

func (r *Reorderer) recordGap(start, end int64, from, to uint32) {
	if end < start {
		return
	}
	g := Gap{StartExt: start, Count: end - start + 1, From: from, To: to}
	g.Blocked = g.Count >= int64(r.cfg.BurstGapThreshold)
	r.gaps = append(r.gaps, g)
}

// Drain returns packets delivered since the previous drain.
func (r *Reorderer) Drain() []Packet {
	out := r.out
	r.out = nil
	return out
}

// Gaps returns all frozen gaps in sequence order.
func (r *Reorderer) Gaps() []Gap { return append([]Gap(nil), r.gaps...) }

// Flush freezes every remaining hole and delivers the tail.
func (r *Reorderer) Flush() {
	if r.nextExpected < 0 && len(r.buf) > 0 {
		r.nextExpected = r.firstExt
	}
	for len(r.buf) > 0 {
		before := len(r.buf)
		r.freezeSmallestHole()
		if len(r.buf) == before {
			// Defensive: never spin if the buffer cannot advance.
			break
		}
	}
}

// Stats flushes the stream and returns the final tail plus statistics.
func (r *Reorderer) Stats() ([]Packet, StreamStats) {
	r.Flush()
	tail := r.Drain()
	stats := StreamStats{
		Received: r.dups + r.lateN + len(r.seen),
		Unique:   len(r.seen), Duplicates: r.dups, Reordered: r.reord,
		Late: r.lateN, LatePackets: append([]int64(nil), r.late...),
		Wrapped: r.wrapped, CommittedExt: r.anchor,
		FirstExt: r.firstExt, LastExt: r.anchor, Gaps: r.Gaps(),
	}
	if r.hasFirst {
		span := r.anchor - r.firstExt + 1
		if lost := span - int64(len(r.seen)); lost > 0 {
			stats.Lost = int(lost)
		}
	}
	return tail, stats
}

// -------------------- bandwidth --------------------

// BandwidthSample is one observed delivery rate over a time slice.
type BandwidthSample struct {
	// AtMillis is the sample's media/arrival time.
	AtMillis int64
	// DeliveryKbps is the measured throughput.
	DeliveryKbps float64
	// SuggestedKbps is the encoder-side suggestion that produced the sample.
	SuggestedKbps float64
}

// BandwidthConfig configures the sliding-window estimator.
type BandwidthConfig struct {
	WindowSamples int
	// MinSamples suppresses adaptation until enough samples exist, so a short
	// burst cannot trigger an erroneous downshift.
	MinSamples int
	// DefaultTargetKbps is used until the window is warm.
	DefaultTargetKbps float64
	// MaxStepPct caps how far one update may move the target.
	MaxStepPct float64
	// HeadroomPct keeps the target below measured capacity.
	HeadroomPct float64
	// HysteresisPct ignores changes smaller than this fraction.
	HysteresisPct float64
}

// DefaultBandwidthConfig returns conservative defaults.
func DefaultBandwidthConfig() BandwidthConfig {
	return BandwidthConfig{
		WindowSamples: 5, MinSamples: 3, DefaultTargetKbps: 1500,
		MaxStepPct: 25, HeadroomPct: 2, HysteresisPct: 2,
	}
}

// BandwidthSampleRecord is the per-tick record required by the manifest:
// target, actual, and signed deviation.
type BandwidthSampleRecord struct {
	AtMillis      int64
	TargetKbps    float64
	ActualKbps    float64
	ErrorPct      float64
	WindowAvgKbps float64
	Warm          bool
}

// BandwidthEstimator adapts the target bitrate from a sliding window.
type BandwidthEstimator struct {
	cfg     BandwidthConfig
	samples []BandwidthSample
	target  float64
	records []BandwidthSampleRecord
}

// NewBandwidthEstimator creates an estimator.
func NewBandwidthEstimator(cfg BandwidthConfig) *BandwidthEstimator {
	if cfg.WindowSamples <= 0 {
		cfg.WindowSamples = 5
	}
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = 3
	}
	if cfg.DefaultTargetKbps <= 0 {
		cfg.DefaultTargetKbps = 1500
	}
	return &BandwidthEstimator{cfg: cfg, target: cfg.DefaultTargetKbps}
}

// Target is the current target bitrate in kbps.
func (b *BandwidthEstimator) Target() float64 { return b.target }

// Add ingests a sample and returns the record for this tick. Before the window
// has MinSamples the target is held; the sample is recorded but marked not
// warm, so a short sample cannot cause an erroneous downshift.
func (b *BandwidthEstimator) Add(s BandwidthSample) BandwidthSampleRecord {
	b.samples = append(b.samples, s)
	if len(b.samples) > b.cfg.WindowSamples {
		b.samples = b.samples[len(b.samples)-b.cfg.WindowSamples:]
	}
	avg := b.windowAverage()
	warm := len(b.samples) >= b.cfg.MinSamples
	if warm {
		desired := avg * (1 - b.cfg.HeadroomPct/100)
		if desired < 1 {
			desired = 1
		}
		// Hysteresis applies to the measured change: small fluctuations in the
		// window average do not cause a retarget. MaxStepPct then bounds how
		// far a single update may move the target.
		if rel := math.Abs(desired-b.target) / b.target; rel > b.cfg.HysteresisPct/100 {
			maxStep := b.target * b.cfg.MaxStepPct / 100
			delta := math.Max(-maxStep, math.Min(maxStep, desired-b.target))
			b.target += delta
		}
	}
	rec := BandwidthSampleRecord{
		AtMillis: s.AtMillis, TargetKbps: b.target, ActualKbps: s.DeliveryKbps,
		WindowAvgKbps: avg, Warm: warm,
	}
	// Deviation is target relative to actual: positive means the target
	// exceeds what the link delivered.
	if s.DeliveryKbps > 0 {
		rec.ErrorPct = (b.target - s.DeliveryKbps) / s.DeliveryKbps * 100
	}
	b.records = append(b.records, rec)
	return rec
}

func (b *BandwidthEstimator) windowAverage() float64 {
	if len(b.samples) == 0 {
		return b.target
	}
	sum := 0.0
	for _, s := range b.samples {
		sum += s.DeliveryKbps
	}
	return sum / float64(len(b.samples))
}

// Records returns every per-tick record.
func (b *BandwidthEstimator) Records() []BandwidthSampleRecord {
	return append([]BandwidthSampleRecord(nil), b.records...)
}

// BandwidthSummary is the aggregate bandwidth record.
type BandwidthSummary struct {
	TargetKbps      float64
	MeanTargetKbps  float64
	MeanActualKbps  float64
	MeanAbsErrorPct float64
	MaxAbsErrorPct  float64
	Records         int
}

// Summary aggregates the bandwidth trace for the CLI and manifest.
func (b *BandwidthEstimator) Summary() BandwidthSummary {
	s := BandwidthSummary{TargetKbps: b.target, Records: len(b.records)}
	if len(b.records) == 0 {
		return s
	}
	var targetSum, actualSum, errSum, maxErr float64
	for _, r := range b.records {
		targetSum += r.TargetKbps
		actualSum += r.ActualKbps
		errSum += math.Abs(r.ErrorPct)
		if math.Abs(r.ErrorPct) > maxErr {
			maxErr = math.Abs(r.ErrorPct)
		}
	}
	n := float64(len(b.records))
	s.MeanTargetKbps = targetSum / n
	s.MeanActualKbps = actualSum / n
	s.MeanAbsErrorPct = errSum / n
	s.MaxAbsErrorPct = maxErr
	s.TargetKbps = b.records[len(b.records)-1].TargetKbps
	return s
}

// String renders the estimator target for diagnostics.
func (s BandwidthSummary) String() string {
	return fmt.Sprintf("target=%.1fkbps actual=%.1fkbps err=%.2f%%", s.TargetKbps, s.MeanActualKbps, s.MeanAbsErrorPct)
}
