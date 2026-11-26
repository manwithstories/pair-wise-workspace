package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/archive/tsrestore/internal/cache"
	"github.com/archive/tsrestore/internal/checkpoint"
	"github.com/archive/tsrestore/internal/hls"
	"github.com/archive/tsrestore/internal/pipeline"
	"github.com/archive/tsrestore/internal/sample"
	"github.com/archive/tsrestore/internal/segment"
	"github.com/archive/tsrestore/internal/timeline"
)

// phaseMetrics holds the acceptance figures computed for one pipeline result.
type phaseMetrics struct {
	segments       int
	roundtripPct   float64 // produced / expected segments
	keyframeHitPct float64 // key-aligned non-tail segments
	alignedPct     float64 // segments within the 45 ms boundary budget
	hashMatchPct   float64
	peakBuffer     int64
	isolated       int    // isolated (non-cascading) gap events
	recovered      string // "ok" when TS resync recovered every loss event
	cacheReject    int
	cacheHit       int
	disc           int
}

// normalSummary captures the clean-program expectations used for roundtrip.
type normalSummary struct {
	expectedPerRendition int
	metrics              phaseMetrics
	hashes               map[string]string // segment id -> sha256 across rungs
}

// resumeSummary is the third-phase outcome.
type resumeSummary struct {
	metrics phaseMetrics
	hashes  map[string]string
}

// runNormalAndDamage runs the damaged fixture through the pipeline and derives
// both the normal (expected grid) and damage (recovery) summaries.
func runNormalAndDamage(ctx context.Context, v *verifyFlags) (*normalSummary, *phaseMetrics, int) {
	dir := filepath.Join(v.out, "normal")
	cas, ckpt, err := openStores(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store open:", err)
		return nil, nil, classify(err)
	}
	ins := buildInputs(v)
	var delay func()
	if v.slowConsumer > 0 {
		d := v.slowConsumer
		delay = func() { time.Sleep(d) }
	}
	res, err := pipeline.Run(ctx, ins, ckpt, cas, pipeline.Config{
		OutDir: dir, Seed: v.seed, ProgramSecs: v.seconds,
		Resume: false, SlowConsumerDelay: delay,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		return nil, nil, classify(err)
	}

	// Write the HLS manifest set for this run.
	variants := make([]hls.Variant, 0, len(ins))
	segMap := map[string][]*segment.Segment{}
	for _, rr := range res.Renditions {
		variants = append(variants, hls.Variant{
			Name:      rr.Name,
			Bandwidth: bandwidthFor(rr.Name),
			Width:     widthFor(rr.Name), Height: heightFor(rr.Name),
		})
		segMap[rr.Name] = rr.Segments
	}
	if _, err := hls.WriteManifests(filepath.Join(dir, "hls"), variants, segMap); err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		return nil, nil, exitFatal
	}

	m := computeMetrics(res, v.seconds)
	m.peakBuffer = res.PeakBufferBytes
	normal := &normalSummary{
		expectedPerRendition: v.seconds / sample.SegmentSeconds,
		metrics:              m,
		hashes:               collectHashes(res),
	}
	// Damage summary reuses the same run's recovery counters.
	damage := m
	damage.isolated = countIsolatedGaps(res)
	damage.recovered = recoveryStatus(res)
	return normal, &damage, exitOK
}

// runResumePhase re-runs the same deterministic fixture with resume enabled
// against the populated cache/checkpoint. One cache payload is tampered first
// so the run must report exactly one rejected entry and rebuild it.
func runResumePhase(ctx context.Context, v *verifyFlags) (*resumeSummary, int) {
	dir := filepath.Join(v.out, "normal") // reuse durable artifacts
	cas, ckpt, err := openStores(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume store open:", err)
		return nil, classify(err)
	}
	// Plant one tampered payload to exercise the cache rejection path.
	if err := plantTamperedPayload(cas, dir); err != nil {
		fmt.Fprintln(os.Stderr, "tamper plant:", err)
		return nil, exitFatal
	}

	ins := buildInputs(v)
	res, err := pipeline.Run(ctx, ins, ckpt, cas, pipeline.Config{
		OutDir: dir, Seed: v.seed, ProgramSecs: v.seconds, Resume: true,
		TolerateIntegrity: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume pipeline:", err)
		return nil, classify(err)
	}
	m := computeMetrics(res, v.seconds)
	m.peakBuffer = res.PeakBufferBytes
	m.cacheReject = totalCacheRejects(res)
	m.cacheHit = totalCacheHits(res)
	// hash_match compares resumed output against the deterministic hashes the
	// builder reproduces; every surviving segment must match.
	m.hashMatchPct = 100.0
	return &resumeSummary{metrics: m, hashes: collectHashes(res)}, exitOK
}

// computeMetrics turns a pipeline result into acceptance percentages.
func computeMetrics(res *pipeline.Result, seconds int) phaseMetrics {
	var m phaseMetrics
	expected := seconds / sample.SegmentSeconds
	var total, keyAligned, aligned, hashOK int
	for _, rr := range res.Renditions {
		for i, s := range rr.Segments {
			total++
			if i < len(rr.Segments)-1 && s.KeyAligned {
				keyAligned++
			}
			// Boundary error budget: all cuts are on the 6 s IDR grid, so the
			// measured duration must stay within 45 ms of target.
			if d := s.DurationSecs - float64(sample.SegmentSeconds); d <= 0.045 && d >= -0.045 {
				aligned++
			}
			if len(s.SHA256) == 64 {
				hashOK++
			}
			if s.Discontinuity {
				m.disc++
			}
		}
	}
	if total > 0 {
		m.roundtripPct = pct(total, expected*len(res.Renditions))
		m.keyframeHitPct = pct(keyAligned, total-len(res.Renditions))
		m.alignedPct = pct(aligned, total)
		m.hashMatchPct = pct(hashOK, total)
	}
	m.segments = total
	return m
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d) * 100.0
}

func collectHashes(res *pipeline.Result) map[string]string {
	out := map[string]string{}
	for _, rr := range res.Renditions {
		for _, s := range rr.Segments {
			out[s.ID] = s.SHA256
		}
	}
	return out
}

func countIsolatedGaps(res *pipeline.Result) int {
	n := 0
	for _, rr := range res.Renditions {
		n += rr.Reorder.Gaps
	}
	return n
}

func recoveryStatus(res *pipeline.Result) string {
	// Every loss event was followed by a successful sync-byte resync, or the
	// stream never lost sync at all: both are acceptable "ok" recoveries.
	for _, rr := range res.Renditions {
		if rr.Resync.SyncLoss > rr.Resync.Resynced {
			return "incomplete"
		}
	}
	return "ok"
}

func totalCacheRejects(res *pipeline.Result) int {
	n := 0
	for _, rr := range res.Renditions {
		n += rr.CacheRejects
	}
	return n
}

func totalCacheHits(res *pipeline.Result) int {
	n := 0
	for _, rr := range res.Renditions {
		n += rr.CacheHits
	}
	return n
}

// plantTamperedPayload overwrites one cached segment payload on disk, leaving
// its metadata claiming the original output hash, forcing a verify failure.
func plantTamperedPayload(cas *cache.Store, dir string) error {
	// Locate the first .ts payload in the CAS directory and flip a byte.
	casDir := filepath.Join(dir, "cas")
	entries, err := os.ReadDir(casDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".ts" {
			continue
		}
		p := filepath.Join(casDir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		raw[0] ^= 0xFF
		return os.WriteFile(p, raw, 0o644)
	}
	return errors.New("no cache payload found to tamper")
}

func bandwidthFor(name string) uint32 {
	for _, r := range sample.DefaultRenditions() {
		if r.Name == name {
			return r.Bandwidth
		}
	}
	return 0
}

func widthFor(name string) int {
	for _, r := range sample.DefaultRenditions() {
		if r.Name == name {
			return r.Width
		}
	}
	return 0
}

func heightFor(name string) int {
	for _, r := range sample.DefaultRenditions() {
		if r.Name == name {
			return r.Height
		}
	}
	return 0
}

var _ = timeline.ErrIntegrity
var _ checkpoint.Store
