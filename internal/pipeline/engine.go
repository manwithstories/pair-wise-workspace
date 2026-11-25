// Package pipeline wires parsing, audio transformation and RTP transport
// into one bounded, cancellable engine and produces the final metrics report.
package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"

	"avpipe/internal/adapter"
	"avpipe/internal/media"
	"avpipe/internal/metrics"
)

// MaxCacheBytes is the hard memory ceiling for buffered frame/packet payloads
// (64 MiB per the acceptance constraint). It is a variable so tests can
// exercise the limit path without allocating real 64 MiB payloads.
var MaxCacheBytes int64 = 64 << 20

// Config parameterizes one pipeline run.
type Config struct {
	Frames       int
	Seed         int64
	LossRate     float64
	Burst        int
	Truncate     int
	CorruptEvery int
	// Case forces a named scenario ("clean" implies no defects).
	Case string
	// JitterWindow90k is the RTP wait window on the 90 kHz clock.
	JitterWindow90k uint64
	// JitterCapacity bounds the jitter buffer queue length.
	JitterCapacity int
	// JitterTicks90k is the maximum one-way arrival jitter simulated.
	JitterTicks90k uint64
	// TargetLUFS drives the audio normalizer.
	TargetLUFS float64
}

// DefaultConfig returns sane demo defaults.
func DefaultConfig() Config {
	return Config{
		Frames: 600, Seed: 7, JitterWindow90k: media.Duration90k(40),
		JitterCapacity: 1024, JitterTicks90k: media.Duration90k(15), TargetLUFS: -16,
	}
}

// Engine executes one end-to-end run.
type Engine struct {
	cfg Config
}

// NewEngine builds an engine.
func NewEngine(cfg Config) *Engine { return &Engine{cfg: cfg} }

// Run generates, parses, transforms, transports and measures a stream.
func (e *Engine) Run(ctx context.Context) (*metrics.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, media.NewError("pipeline", media.KindCancelled, 0, media.ActionCancel, "%v", err)
	}
	gen := e.genConfig()
	raw, err := adapter.Generate(gen)
	if err != nil {
		return nil, err
	}
	if err := e.checkCache(int64(len(raw))); err != nil {
		return nil, err
	}

	parsed, err := adapter.ParseContainer(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, media.NewError("pipeline", media.KindCancelled, 0, media.ActionCancel, "%v", err)
	}
	return e.process(ctx, parsed)
}

// genConfig maps pipeline config onto the adapter generator config.
func (e *Engine) genConfig() adapter.GenConfig {
	c := e.cfg
	g := adapter.DefaultGenConfig(c.Frames, c.Seed)
	if c.Case == "clean" {
		return g
	}
	g.TailTruncate = c.Truncate
	g.CorruptEvery = c.CorruptEvery
	return g
}

// process runs the audio and RTP stages over parsed frames and aggregates.
func (e *Engine) process(ctx context.Context, parsed *adapter.ParseResult) (*metrics.Report, error) {
	rep := &metrics.Report{
		Frames:        len(parsed.Frames),
		BadRecords:    countBad(parsed.Frames),
		IndexFallback: parsed.UsedFallback,
		ParserErrors:  len(parsed.Errors),
	}

	// Audio stage: resample, downmix, normalize every aligned block, and
	// propagate frame gaps so missing audio is counted.
	audioOut, audioGaps, err := e.runAudio(ctx, parsed)
	if err != nil {
		return nil, err
	}
	rep.AudioBlocks = len(audioOut)
	rep.AudioGaps = audioGaps

	// RTP stage over non-corrupt frames.
	latencies, rtpCounters, err := e.runRTP(ctx, parsed.Frames)
	if err != nil {
		return nil, err
	}
	rep.LostPackets = rtpCounters.lost
	rep.LostMedia = rtpCounters.lostMedia
	rep.RecoveredFEC = rtpCounters.recovered
	rep.Unrecoverable = rtpCounters.unrecoverable
	rep.BufferOverflows = rtpCounters.overflows
	rep.LateDrops = rtpCounters.late

	rep.RecoveredPct = e.recoveryPct(parsed, rtpCounters)
	rep.SyncMaxMillis = e.syncMax(parsed, audioOut)
	rep.LatencyP95Ms = metrics.Percentile(latencies, 95)
	rep.OutputSHA256 = e.outputHash(parsed, audioOut)
	return rep, nil
}

// checkCache enforces the 64 MiB buffering ceiling with a locatable error.
func (e *Engine) checkCache(need int64) error {
	if need > MaxCacheBytes {
		return media.NewError("pipeline", media.KindLimitExceeded, 0,
			media.ActionLimit, "input %d bytes exceeds cache ceiling %d", need, MaxCacheBytes)
	}
	return nil
}

func countBad(frames []media.Frame) int {
	n := 0
	for _, f := range frames {
		if f.Corrupt {
			n++
		}
	}
	return n
}

// recoveryPct is the share of expected good frames that survive as playable
// media after loss and FEC reconstruction. Parser-level corrupt frames are
// excluded from the transport denominator.
func (e *Engine) recoveryPct(parsed *adapter.ParseResult, c rtpCounters) float64 {
	good := len(parsed.Frames) - countBad(parsed.Frames)
	if good == 0 {
		return 0
	}
	playable := good - c.unrecoverable
	if playable < 0 {
		playable = 0
	}
	return 100.0 * float64(playable) / float64(good)
}

// outputHash is the deterministic SHA-256 over delivered frame payloads in
// sequence order followed by the processed audio samples. Same seed => same
// hash across runs.
func (e *Engine) outputHash(parsed *adapter.ParseResult, audioOut []processedAudio) string {
	h := sha256.New()
	for _, f := range parsed.Frames {
		if f.Corrupt {
			continue
		}
		h.Write(f.Payload)
	}
	var buf [2]byte
	for _, a := range audioOut {
		for _, s := range a.samples {
			buf[0] = byte(uint16(s) >> 8)
			buf[1] = byte(uint16(s))
			h.Write(buf[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
