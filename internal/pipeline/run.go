// Package pipeline wires demuxing, mapping, timeline, the bounded pool,
// deterministic merge, checkpointing and remuxing into one runnable media
// processing flow. It is the single place where subsystem boundaries meet, and
// it always returns a structured Result rather than printing itself.
package pipeline

import (
	"context"
	"time"

	"mediacore/internal/checkpoint"
	"mediacore/internal/clock"
	"mediacore/internal/demux"
	"mediacore/internal/fault"
	"mediacore/internal/fixture"
	"mediacore/internal/merge"
	"mediacore/internal/sample"
	"mediacore/internal/track"
)

// Config controls one pipeline run.
type Config struct {
	Seed     int64
	Variant  int
	Mode     fixture.Mode
	Workers  int
	QueueCap int
	// Checkpoint path and output location.
	Checkpoint  string
	OutPath     string
	InputDigest string
	// WorkerGate, when non-nil, blocks each worker until it closes or ctx is
	// cancelled. It exists to deterministically exercise a full-queue cancel.
	WorkerGate <-chan struct{}
	// WorkerDelay adds a bounded per-job sleep (honoring ctx) so cancellation
	// can land mid-processing on fast machines.
	WorkerDelay time.Duration
}

// Result is the JSON-serializable acceptance record.
type Result struct {
	Status           string `json:"status"`
	Tracks           int    `json:"tracks"`
	Samples          int    `json:"samples"`
	FirstErrorOffset int64  `json:"first_error_offset"`
	OffsetErrorBytes int64  `json:"offset_error_bytes"`
	ResumeMS         int64  `json:"resume_ms"`
	SHA256           string `json:"sha256"`
	OrderedEqual     bool   `json:"ordered_equal"`
	// UncommittedSeq is the boundary not yet committed when cancelled.
	UncommittedSeq int `json:"uncommitted_seq,omitempty"`
}

// DefaultConfig returns the demo configuration for a seed.
func DefaultConfig(seed int64, workers int, ckpt, out string) Config {
	return Config{
		Seed: seed, Variant: 0, Mode: fixture.ModeOK, Workers: workers,
		QueueCap: workers * 4, Checkpoint: ckpt, OutPath: out,
		InputDigest: digestOfSeed(seed),
	}
}

// Run executes the full flow. A context cancellation yields status=cancelled;
// recoverable input faults are reported in the result while structural faults
// are returned as an error for the caller to map to exit code 1.
func Run(ctx context.Context, cfg Config) (Result, error) {
	res := Result{Status: "ok", FirstErrorOffset: -1, OrderedEqual: true}

	art, err := fixture.Build(fixture.Spec{
		Seed: cfg.Seed, Variant: cfg.Variant,
		Tracks: fixture.DefaultSpec(cfg.Seed, cfg.Variant).Tracks,
		Mode:   cfg.Mode, CorruptIndex: 10,
	})
	if err != nil {
		return res, err
	}

	// --- Demux -----------------------------------------------------------
	reader, err := demux.NewReader(bytesReader(art.Bytes), demux.DefaultLimits())
	if err != nil {
		return res, mapFault(err)
	}
	container, err := reader.Open()
	if err != nil {
		return res, mapFault(err) // structural: caller exits 1
	}
	all, recovered, err := container.ReadAll()
	if err != nil {
		return res, mapFault(err)
	}
	res.Tracks = len(container.Tracks)
	if f := firstFault(recovered); f != nil {
		res.FirstErrorOffset = f.Offset
		if art.CorruptOffset >= 0 {
			res.OffsetErrorBytes = abs64(f.Offset - art.CorruptOffset)
		}
	}

	// --- Flatten + map + timeline ---------------------------------------
	var flat []*sample.Sample
	for _, ss := range all {
		flat = append(flat, ss...)
	}
	mapping := track.DefaultMapping(res.Tracks)
	flat = mapping.Apply(flat)
	// Default plan retains everything; whole-sample selection still runs so
	// the boundary rule is exercised.
	sel := timelineSelect(flat)

	// --- Checkpoint / resume --------------------------------------------
	store, err := checkpoint.NewStore(cfg.Checkpoint)
	if err != nil {
		return res, err
	}
	resumeStart := time.Now()
	state := store.LoadOrRollback(
		checkpoint.State{Version: 1, PerTrack: map[uint32]int{}, InputDigest: cfg.InputDigest},
		func(*fault.Fault) {})
	res.ResumeMS = time.Since(resumeStart).Milliseconds()
	startSeq := state.CommittedInputSeq

	// --- Bounded pool + deterministic merge -----------------------------
	ordered, cancelled, uncommitted, err := processThroughPool(ctx, cfg, sel, startSeq)
	if err != nil {
		return res, mapFault(err)
	}
	if cancelled {
		res.Status = "cancelled"
		res.UncommittedSeq = uncommitted
		res.OrderedEqual = true
		return res, nil
	}

	// Verify concurrency-independent ordering against a sequential reference.
	ref := referenceOrder(sel, startSeq)
	res.OrderedEqual = merge.Equal(ref, ordered)
	res.Samples = len(ordered)

	// --- Remux + checksum ------------------------------------------------
	metrics, err := remuxTo(cfg, ordered)
	if err != nil {
		return res, err
	}
	res.SHA256 = metrics.SHA256

	// Commit the final boundary atomically.
	finalState := checkpoint.State{
		Version: 1, CommittedInputSeq: startSeq + len(sel),
		CommittedSamples: state.CommittedSamples + len(sel),
		PerTrack:         metrics.PerTrack, ResumeTick: int64(lastPTS(ordered)),
		InputDigest: cfg.InputDigest,
	}
	if err := store.Save(finalState); err != nil {
		return res, err
	}
	return res, nil
}

func firstFault(fs []*fault.Fault) *fault.Fault {
	if len(fs) == 0 {
		return nil
	}
	return fault.First(fs)
}

func mapFault(err error) error {
	if f := fault.As(err); f != nil && f.Strategy == fault.Abort {
		return &StructuralError{Fault: f}
	}
	return err
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func lastPTS(ss []*sample.Sample) clock.Tick {
	if len(ss) == 0 {
		return 0
	}
	return ss[len(ss)-1].PTS
}

// StructuralError marks an unrecoverable input fault (process exit code 1).
type StructuralError struct{ Fault *fault.Fault }

func (e *StructuralError) Error() string { return e.Fault.Error() }
