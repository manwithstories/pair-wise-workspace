// Package pipeline orchestrates ingest, jitter buffering, recovery,
// the watermark timeline and normalization into one deterministic
// engine, and produces the run summary.
package pipeline

import (
	"mediad/internal/checkpoint"
	"mediad/internal/ingest"
	"mediad/internal/jitter"
	"mediad/internal/media"
	"mediad/internal/normalize"
	"mediad/internal/recovery"
	"mediad/internal/timeline"
)

// Config tunes the engine.
type Config struct {
	RewindWindowNanos int64 // late data inside this window triggers replay
	DrainSlackNanos   int64 // emission lag behind the newest pushed time
	CheckpointEvery   int   // arrivals between checkpoints, 0 disables
	MaxGapFill        int   // gaps longer than this are unrecoverable
}

// Summary is the run report printed by the CLI. LocErrorPackets,
// RestartMs and ChecksumSHA256 are filled in by the caller.
type Summary struct {
	InputPackets    int64    `json:"input_packets"`
	Loss            int64    `json:"loss"`
	Reordered       int64    `json:"reordered"`
	Duplicates      int64    `json:"duplicates"`
	Unrecoverable   int64    `json:"unrecoverable"`
	LayoutErrors    int64    `json:"layout_errors"`
	LocatedPacket   int64    `json:"located_packet"`
	LocErrorPackets int64    `json:"loc_error_packets"`
	Rewinds         int64    `json:"rewinds"`
	RestartMs       int64    `json:"restart_ms"`
	ChecksumSHA256  string   `json:"checksum_sha256"`
	Faults          int64    `json:"faults"`
	Diagnostics     []string `json:"diagnostics,omitempty"`
}

// Engine is the deterministic media recovery pipeline.
type Engine struct {
	cfg        Config
	reg        *ingest.Registry
	queues     map[uint32]*jitter.Queue
	recov      map[uint32]*recovery.Tracker
	clocks     map[uint32]*ingest.ClockMap
	resamp     map[uint32]*normalize.Resampler
	colors     *normalize.ColorRegistry
	tl         *timeline.Timeline
	sink       timeline.Sink
	store      *checkpoint.Store
	trackOrder []uint32 // sorted SSRCs for deterministic iteration

	arrival   int64
	lastPush  map[uint32]int64 // newest pushed event time per track
	maxPushed int64            // min over tracks: time up to which all tracks delivered
	pending   map[uint32][]pendingItem
	gen       uint64
	counters  checkpoint.Counters
	faults    int64
	ckptErr   error
}

// pendingItem is a packet held until its track's clock is anchored by
// the first Sender Report.
type pendingItem struct {
	pkt     *ingest.Packet
	fault   *media.Fault // recovery fault observed at ingest, if any
	gapFill bool
}

func New(cfg Config, sink timeline.Sink, store *checkpoint.Store) *Engine {
	if cfg.RewindWindowNanos <= 0 {
		cfg.RewindWindowNanos = 2 * media.NanosPerSecond
	}
	if cfg.DrainSlackNanos <= 0 {
		cfg.DrainSlackNanos = 100 * int64(1_000_000)
	}
	e := &Engine{
		cfg:      cfg,
		reg:      ingest.NewRegistry(),
		queues:   make(map[uint32]*jitter.Queue),
		recov:    make(map[uint32]*recovery.Tracker),
		clocks:   make(map[uint32]*ingest.ClockMap),
		resamp:   make(map[uint32]*normalize.Resampler),
		colors:   normalize.NewColorRegistry(),
		sink:     sink,
		store:    store,
		lastPush: make(map[uint32]int64),
		pending:  make(map[uint32][]pendingItem),
	}
	e.counters.FirstFault = -1
	e.tl = timeline.NewTimeline(timeline.NewWatermark(cfg.RewindWindowNanos), sink)
	return e
}

// RegisterTrack adds a track from signaling.
func (e *Engine) RegisterTrack(p ingest.TrackParams) {
	e.reg.Register(p)
	e.queues[p.SSRC] = jitter.New(p.SSRC, 0, 0)
	e.recov[p.SSRC] = recovery.New(p.SSRC, e.cfg.MaxGapFill)
	e.clocks[p.SSRC] = &ingest.ClockMap{SSRC: p.SSRC, Rate: p.ClockRate}
	e.resamp[p.SSRC] = normalize.NewResampler(p.ClockRate, normalize.TargetRate)
	e.trackOrder = insertSorted(e.trackOrder, p.SSRC)
}

// UpdateTrack replaces the signaling parameters of a known track,
// e.g. after a renegotiation. Reconstruction state is kept.
func (e *Engine) UpdateTrack(p ingest.TrackParams) { e.reg.Register(p) }

// ObserveColor folds newly signaled color metadata into the registry.
func (e *Engine) ObserveColor(ssrc uint32, c normalize.ColorInfo) {
	if f := e.colors.Observe(ssrc, c); f != nil {
		f.At = e.arrival
		e.noteFault(f)
	}
}

func insertSorted(s []uint32, v uint32) []uint32 {
	i := 0
	for i < len(s) && s[i] < v {
		i++
	}
	return append(s[:i], append([]uint32{v}, s[i:]...)...)
}

func (e *Engine) noteFault(f *media.Fault) {
	if f == nil {
		return
	}
	e.faults++
	if e.counters.FirstFault < 0 {
		e.counters.FirstFault = f.At
	}
}

// Arrival returns the number of input packets processed so far.
func (e *Engine) Arrival() int64 { return e.arrival }

// ProcessRTP ingests one raw RTP datagram.
func (e *Engine) Summary() Summary {
	return Summary{
		InputPackets:  e.arrival,
		Loss:          e.counters.Loss,
		Reordered:     e.counters.Reordered,
		Duplicates:    e.counters.Duplicates,
		Unrecoverable: e.counters.Unrecoverable,
		LayoutErrors:  e.counters.LayoutErrors,
		LocatedPacket: e.counters.FirstFault,
		Rewinds:       e.tl.Rewinds,
		Faults:        e.faults,
		Diagnostics:   e.colors.Diagnostics(),
	}
}
