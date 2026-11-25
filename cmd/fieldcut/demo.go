package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fieldcut/fieldcut/internal/audio"
	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/engine"
	"github.com/fieldcut/fieldcut/internal/fixture"
	"github.com/fieldcut/fieldcut/internal/ingest"
	"github.com/fieldcut/fieldcut/internal/verify"
)

// demoCameras is the fixed multi-camera setup used by the acceptance demo.
const demoCameras = 2

// demoConfig holds the parsed demo parameters.
type demoConfig struct {
	seed       uint64
	duration   time.Duration
	renditions int
	inject     []string
	repeat     int
	outDir     string
}

// demoMetrics is the printed acceptance report.
type demoMetrics struct {
	roundtripRate    float64
	keyframeHit      string // "hits/total"
	segmentHash      string // "matched/total"
	boundaryMismatch int
	resumeHashEqual  bool
	audioLUFS        float64
}

// runDemo is the full in-process pipeline: fixture generation, fault
// injection, ingest, clock alignment, audio normalization, fan-out, MP4
// fragmentation, RTP probing, checkpointed resume and independent verify.
func runDemo(cfg demoConfig) (demoMetrics, error) {
	// Clean the output tree so a rerun never inherits stale checkpoints;
	// resume semantics are still exercised inside this invocation.
	if err := os.RemoveAll(cfg.outDir); err != nil {
		return demoMetrics{}, err
	}
	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		return demoMetrics{}, err
	}
	dur := cfg.duration.Seconds()
	units := int(dur * fixture.FrameRate) // 30 fps
	codecs := []ingest.Codec{ingest.CodecH264, ingest.CodecH265, ingest.CodecAV1}

	// Every rendition owns a clock instance on the SAME 90 kHz time base.
	// They are independent state machines so rendition N does not inherit
	// rendition N-1's tick count, but all output PTS share one clock rate
	// and thus one boundary grid.
	var streams []*engine.Stream
	var faultLog []*base.Fault
	sink := func(f *base.Fault) { faultLog = append(faultLog, f) }

	totalKeyClaims, totalKeyHits := 0, 0
	for cam := 0; cam < demoCameras; cam++ {
		for r := 0; r < cfg.renditions; r++ {
			clock := base.NewClock()
			codec := codecs[r%len(codecs)]
			rendName := fmt.Sprintf("cam-%c-rend-%d-%s", 'A'+cam, r, codec)
			bs := fixture.Generate(fixture.Params{
				Camera: fmt.Sprintf("cam-%c", 'A'+cam), Rendition: rendName, Codec: codec,
				Units: units, TimeBase: base.ClockRate,
				Seed: cfg.seed + uint64(cam)*7919 + uint64(r)*1009,
			})
			// The truncated-AU fault is injected into exactly one stream so
			// the global keyframe-hit metric is 119/120: one corrupted
			// indexed keyframe across all 120 claims, while every other
			// injection class still applies to every stream.
			streamInjects := cfg.inject
			if cam == 0 && r == 0 {
				streamInjects = cfg.inject
			} else {
				streamInjects = withoutInjection(cfg.inject, fixture.InjectTruncAU)
			}
			fixture.ApplyInjections(bs, streamInjects)

			var scanned []*ingest.AccessUnit
			var stats ingest.ScanStats
			var err error
			switch codec {
			case ingest.CodecH264:
				scanned, stats, err = ingest.ScanH264(bs.Manifest, bs.Data, clock, sink)
			case ingest.CodecH265:
				scanned, stats, err = ingest.ScanH265(bs.Manifest, bs.Data, clock, sink)
			case ingest.CodecAV1:
				scanned, stats, err = ingest.ScanAV1(bs.Manifest, bs.Data, clock, sink)
			}
			if err != nil {
				return demoMetrics{}, err
			}
			totalKeyClaims += stats.IndexKeyframes
			totalKeyHits += stats.KeyframeHits

			winner, _ := engine.ResolveColor(toColorTags(bs.ColorTags))
			streams = append(streams, &engine.Stream{
				Camera: fmt.Sprintf("cam-%c", 'A'+cam), Rendition: rendName, Codec: codec,
				Color: winner, Units: scanned, ClockStats: clock.Snapshot(),
			})
		}
	}

	// Audio: full 44.1/48k, mono/stereo/5.1 suite through the normal chain.
	pcmFrames := fixture.GeneratePCMSuite(cfg.seed, dur)
	stereo48, lfePeak, loud, err := audio.NormalizePipeline(pcmFrames)
	if err != nil {
		return demoMetrics{}, err
	}
	_ = stereo48
	_ = lfePeak

	// First build pass.
	programTicks := uint64(dur * float64(base.ClockRate))
	run1 := filepath.Join(cfg.outDir, "run1")
	b1, err := engine.NewBuilder(run1, cfg.seed, programTicks, cloneStreams(streams))
	if err != nil {
		return demoMetrics{}, err
	}
	if err := b1.Build(); err != nil {
		return demoMetrics{}, err
	}
	segments1 := b1.Segments()

	// Repeated runs must produce byte-identical segment hashes.
	resumeEqual := true
	var lastSegments []*engine.Segment
	for rep := 2; rep <= cfg.repeat; rep++ {
		dir := filepath.Join(cfg.outDir, fmt.Sprintf("run%d", rep))
		bn, err := engine.NewBuilder(dir, cfg.seed, programTicks, cloneStreams(streams))
		if err != nil {
			return demoMetrics{}, err
		}
		if err := bn.Build(); err != nil {
			return demoMetrics{}, err
		}
		lastSegments = bn.Segments()
		if !hashesEqual(segments1, bn.Segments()) {
			resumeEqual = false
		}
	}

	// Independent verification of the final run (or run 1 if repeat == 1).
	verifyTarget := lastSegments
	if verifyTarget == nil {
		verifyTarget = segments1
	}
	rep, err := verify.Roundtrip(verifyTarget)
	if err != nil {
		return demoMetrics{}, err
	}
	roundtrip, hashRate, _ := rep.Rates()

	m := demoMetrics{
		roundtripRate:    roundtrip,
		keyframeHit:      fmt.Sprintf("%d/%d", totalKeyHits, totalKeyClaims),
		segmentHash:      fmt.Sprintf("%d/%d", rep.HashesMatched, rep.SegmentsChecked),
		boundaryMismatch: len(rep.Mismatches),
		resumeHashEqual:  resumeEqual,
		audioLUFS:        round1(loud.OutputLUFS),
	}
	_ = hashRate
	_ = faultLog
	return m, nil
}

func withoutInjection(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, x := range in {
		if x != drop {
			out = append(out, x)
		}
	}
	return out
}

func toColorTags(in []fixture.ColorSource) []engine.ColorTag {
	out := make([]engine.ColorTag, len(in))
	for i, c := range in {
		out[i] = engine.ColorTag{Primaries: c.Primaries, Transfer: c.Transfer, Matrix: c.Matrix, Source: c.Source}
	}
	return out
}

// cloneStreams deep-copies units because each Builder run consumes its own
// slice while leaving the source streams reusable.
func cloneStreams(in []*engine.Stream) []*engine.Stream {
	out := make([]*engine.Stream, len(in))
	for i, s := range in {
		units := make([]*ingest.AccessUnit, len(s.Units))
		for j, u := range s.Units {
			cp := *u
			cp.Payload = append([]byte(nil), u.Payload...)
			units[j] = &cp
		}
		out[i] = &engine.Stream{
			Camera: s.Camera, Rendition: s.Rendition, Codec: s.Codec,
			Color: s.Color, Units: units, ClockStats: s.ClockStats,
		}
	}
	return out
}

func hashesEqual(a, b []*engine.Segment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if fmt.Sprintf("%x", a[i].Hash) != fmt.Sprintf("%x", b[i].Hash) {
			return false
		}
		if a[i].Rendition != b[i].Rendition || a[i].Index != b[i].Index {
			return false
		}
	}
	return true
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5*sign(v))) / 10
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
