package pipeline

import (
	"encoding/binary"
	"sort"

	"mediakernel/internal/bandwidth"
	"mediakernel/internal/clock"
	"mediakernel/internal/jitter"
	"mediakernel/internal/model"
	"mediakernel/internal/recovery"
	"mediakernel/internal/rtp"
	"mediakernel/internal/sync"
	"mediakernel/internal/timeline"
)

// Engine is a reusable, allocation-bounded media processing kernel. Construct
// once per configuration and invoke Run for any number of deterministic
// scenario replays.
type Engine struct {
	cfg       Config
	log       *model.Log
	tracker   *rtp.Tracker
	window    *jitter.Window
	watermark *timeline.Watermark
	replayer  *timeline.Replayer
	recoverer *recovery.Recoverer
	aligner   *sync.Aligner
	estimator *bandwidth.Estimator
	latencies []int64
	outputSeq int
}

// New builds every stage from cfg and validates the policy bounds.
func New(cfg Config) *Engine {
	if cfg.FramesPerPacket <= 0 || cfg.Channels <= 0 {
		cfg = DefaultConfig(cfg.Scenario)
	}
	log := model.NewLog(cfg.FaultLogLimit)
	cfg.Recovery.Channels = cfg.Channels
	cfg.Recovery.FramesPerPacket = cfg.FramesPerPacket
	return &Engine{
		cfg:       cfg,
		log:       log,
		tracker:   rtp.NewTracker(log, 4096),
		window:    jitter.New(jitter.Config{MaxEvents: cfg.WindowMaxEvents}, log),
		watermark: timeline.NewWatermark(log),
		replayer:  timeline.NewReplayer(cfg.ReplayWindow, 4096, log),
		recoverer: recovery.New(cfg.Recovery, log),
		aligner:   sync.NewAligner(cfg.Align, log),
		estimator: bandwidth.NewEstimator(cfg.Bandwidth),
	}
}

// Run executes the full pipeline over one deterministic input set.
func (e *Engine) Run(in Input) *Output {
	e.feedBandwidth(in.Bandwidth)
	sealed := e.processAudio(in.AudioEvents)
	block, stats := e.rebuildAudio(sealed)
	packetBlocks := e.splitPackets(block)
	frames := e.alignVideo(in.VideoFrames, packetBlocks)
	return e.assemble(frames, block, stats)
}

// feedBandwidth replays the bandwidth curve as if observed on the wire.
func (e *Engine) feedBandwidth(points []BandwidthPoint) {
	var prev clock.Ticks
	for i, p := range points {
		if i == 0 {
			e.estimator.Observe(0, p.At)
			prev = p.At
			continue
		}
		dt := (p.At - prev).Seconds()
		prev = p.At
		if dt <= 0 {
			continue
		}
		e.estimator.Observe(int(p.BytesPerSec*dt+0.5), p.At)
	}
}

// processAudio pushes arrival-order events through tracker/window/watermark
// and returns the sealed, event-time-ordered audio events.
func (e *Engine) processAudio(events []*model.Event) []*model.Event {
	sealed := make([]*model.Event, 0, len(events))
	lateSet := make(map[uint16]bool)
	var latestArrival clock.Ticks
	seal := func(ev *model.Event) {
		e.watermark.Advance(ev.EventTime)
		e.replayer.Seal(ev.EventTime, ev.Stream, ev.Seq, ev.PayloadHash, e.outputSeq)
		e.outputSeq++
		var latency clock.Ticks
		if lateSet[ev.Seq] {
			// Replayed packets measure real network delay; the normal
			// scheduled-arrival formula would produce negative values.
			latency = ev.Arrival - ev.EventTime
		} else {
			latency = ev.EventTime + e.estimator.TargetBuffer() - ev.Arrival
		}
		e.latencies = append(e.latencies, latency.Milliseconds())
		sealed = append(sealed, ev)
	}
	drain := func(now clock.Ticks) {
		cutoff := now - e.estimator.TargetBuffer()
		for _, ev := range e.window.DrainUpTo(int64(cutoff)) {
			seal(ev)
		}
	}
	for _, ev := range events {
		if ev.Arrival > latestArrival {
			latestArrival = ev.Arrival
		}
		e.estimator.Observe(len(ev.AudioPCM), ev.Arrival)
		if !e.tracker.Observe(ev) {
			continue // duplicate deduplicated or conflict isolated
		}
		if e.watermark.IsSealed(ev.EventTime) {
			d := e.replayer.Offer(ev, e.watermark.Mark())
			if d.Replay {
				// Deterministic recomputation over the exact sealed set;
				// the late event then rejoins as a replayed item.
				_ = e.replayer.Recompute(d.Slot)
				lateSet[ev.Seq] = true
				e.window.Push(ev)
				drain(latestArrival)
				continue
			}
			// Beyond the replay window: the offer decision was logged and
			// the packet is not admitted into the timeline.
			continue
		}
		e.window.Push(ev)
		drain(latestArrival)
	}
	// Flush everything still buffered so no event is silently lost.
	for _, ev := range e.window.DrainUpTo(int64(latestArrival) + 1<<40) {
		seal(ev)
	}
	return sealed
}

// rebuildAudio turns sealed events plus their detected gaps into one
// continuous, origin-marked audio block.
func (e *Engine) rebuildAudio(sealed []*model.Event) (*model.AudioBlock, recovery.Stats) {
	if len(sealed) == 0 {
		return &model.AudioBlock{Channels: e.cfg.Channels, SampleRate: int(clock.AudioClockRate)}, recovery.Stats{}
	}
	sort.Slice(sealed, func(i, j int) bool { return sealed[i].Seq < sealed[j].Seq })
	present := make(map[uint16][]int16, len(sealed))
	minSeq, maxSeq := sealed[0].Seq, sealed[0].Seq
	for _, ev := range sealed {
		present[ev.Seq] = decodePCM16(ev.AudioPCM)
		if ev.Seq < minSeq {
			minSeq = ev.Seq
		}
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}
	slots := make([]recovery.Slot, 0, int(maxSeq-minSeq)+1)
	for seq := minSeq; ; seq++ {
		samples, ok := present[seq]
		slots = append(slots, recovery.Slot{Seq: seq, Present: ok, Samples: samples})
		if seq == maxSeq {
			break
		}
	}
	return e.recoverer.Rebuild(sealed[0].Stream, sealed[0].EventTime, slots)
}

func decodePCM16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}
