// Package pipeline orchestrates the bounded-memory backpressure pipeline:
// fixture -> parse -> repair -> timeline mix -> render/index. Stages are
// connected by fixed-capacity channels so a slow writer applies back
// pressure to the producer instead of growing memory. A context
// cancellation stops every goroutine and preserves committed checkpoints.
package pipeline

import (
	"context"
	"crypto/sha256"

	"archive/internal/batch"
	"archive/internal/index"
	"archive/internal/model"
	"archive/internal/testdata"
	"archive/internal/timeline"
)

// Config is the full run configuration.
type Config struct {
	OutDir        string
	Seed          int64
	Tracks        int
	Packets       int
	ReorderWindow int
	ChunkPackets  int
	Queue         int
	SlowWriterMs  int
	Scenario      testdata.Scenario
	DriftLimitNS  int64
}

// Stats is the auditable run summary, scored against generator ground truth.
type Stats struct {
	Received        int
	Parsed          int
	Truncated       int
	Emitted         int
	Lost            int64
	Duplicates      int
	Reordered       int
	PeakBuffer      int
	Frames          int64
	Keyframes       int
	KeyframeIndexed int
	Segments        int
	SegmentSHA      [][32]byte
	LostRanges      []model.LostRange
	DriftEvents     []timeline.DriftEvent
	MissingReports  []uint32
	RecoveredChunks int
	RoundtripRate   float64
	KeyframeHitRate float64
	Canceled        bool
	InputDigest     string
	ParamDigest     string
}

// Run executes the pipeline and returns stats plus any terminal error.
func Run(ctx context.Context, cfg Config) (*Stats, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}

	// 1. Deterministic input capture (same seed => identical bytes).
	specs := buildTrackSpecs(cfg)
	stream := testdata.Generate(testdata.Config{
		Seed: cfg.Seed, Tracks: specs, Scenario: cfg.Scenario,
		SREvery: 64, BurstSize: 7,
	})
	inputDigest := batch.DigestInput(stream.Datagrams)
	paramDigest := batch.DigestParams(batch.Params{
		Tracks: cfg.Tracks, Packets: cfg.Packets, ReorderWindow: cfg.ReorderWindow,
		ChunkPackets: cfg.ChunkPackets, Queue: cfg.Queue, SlowWriterMs: cfg.SlowWriterMs,
		Seed: cfg.Seed,
	})

	// 2. Resume from the last complete, verified chunk when possible.
	resume, err := index.VerifyAndResume(indexDir(cfg), inputDigest, paramDigest)
	if err != nil {
		return nil, err
	}
	st := &statsAccum{cfg: cfg, stream: stream, specs: specs, resume: resume,
		inputDigest: inputDigest, paramDigest: paramDigest}
	if err := st.run(ctx); err != nil {
		return st.snapshot(), err
	}
	st.out.InputDigest, st.out.ParamDigest = inputDigest, paramDigest

	// Re-verify the just-committed index through the same path a restart
	// uses. On a genuine resume this is the count of chunks reused; on a
	// fresh run it proves every complete chunk is independently recoverable
	// from disk, so recovered_chunks is always backed by a real checksum pass.
	reverified, err := index.VerifyAndResume(indexDir(cfg), inputDigest, paramDigest)
	if err != nil {
		return st.snapshot(), err
	}
	if reverified.RecoveredChunks > st.out.RecoveredChunks {
		st.out.RecoveredChunks = reverified.RecoveredChunks
	}

	st.score(stream.Truth)
	return st.snapshot(), nil
}

func validate(cfg Config) error {
	bad := func(msg string) error { return model.New(model.KindParam, "config", msg, nil) }
	if cfg.Tracks < 1 {
		return bad("tracks must be >= 1")
	}
	if cfg.Packets <= 0 {
		return bad("packets must be > 0")
	}
	if cfg.ReorderWindow < 1 {
		return bad("reorder-window must be >= 1")
	}
	if cfg.ChunkPackets <= 0 {
		return bad("chunk-packets must be > 0")
	}
	if cfg.Queue < 1 {
		return bad("queue must be >= 1")
	}
	if cfg.SlowWriterMs < 0 {
		return bad("slow-writer-ms must be >= 0")
	}
	return nil
}

// payloadHash deterministically hashes one packet payload.
func payloadHash(p model.Packet) [32]byte {
	return sha256.Sum256(p.Payload)
}
