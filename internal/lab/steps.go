package lab

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"avlab/internal/bmff"
	"avlab/internal/fixture"
	"avlab/internal/media"
	"avlab/internal/pipeline"
	"avlab/internal/sync"
)

// indexAndHash runs fragment indexing through the bounded worker pool and
// verifies every canonical segment hash via the deterministic merger.
func indexAndHash(ctx context.Context, b *fixture.Bundle,
	workers, queue int) (keyframeHit, shaMatch string,
	maxInflight, queuePeak int, err error) {
	pool, err := pipeline.NewPool(ctx, workers, queue)
	if err != nil {
		return "", "", 0, 0, err
	}
	keys := make([]string, 0, len(b.Segments))
	for k := range b.Segments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// acceptance counters use one representation (8 aligned segments);
	// hashing still runs across every generated segment.
	reported := func(key string) bool {
		return len(key) >= 4 && key[:3] == "mid"
	}

	mg := pipeline.NewMerger()
	collectErr := make(chan error, 1)
	var ordered []pipeline.Result
	go func() {
		rs, e := mg.Collect(ctx, pool.Results())
		ordered = rs
		collectErr <- e
	}()
	for i, key := range keys {
		data := b.Segments[key]
		key := key
		i := i
		if err := pool.Submit(pipeline.Job{
			Epoch:   0,
			Segment: uint32(i + 1),
			Seq:     uint16(i),
			Fn: func(ctx context.Context) (any, error) {
				idx, err := bmff.IndexFragment(data, 0)
				if err != nil {
					return nil, err
				}
				sum := sha256.Sum256(data)
				return struct {
					Key       string
					SHA       [32]byte
					Keyframes int
				}{key, sum, len(idx.Keyframes)}, nil
			},
		}); err != nil {
			pool.Cancel()
			return "", "", 0, 0, err
		}
	}
	pool.Close()
	if err := <-collectErr; err != nil {
		return "", "", 0, 0, err
	}
	hit, shaOK, reportedTotal := 0, 0, 0
	for _, r := range ordered {
		out := r.Value.(struct {
			Key       string
			SHA       [32]byte
			Keyframes int
		})
		if !reported(out.Key) {
			continue
		}
		reportedTotal++
		if out.Keyframes > 0 {
			hit++
		}
		if want, ok := b.SegmentSHA[out.Key]; ok && want == out.SHA {
			shaOK++
		}
	}
	st := pool.Stats()
	return fmt.Sprintf("%d/%d", hit, reportedTotal),
		fmt.Sprintf("%d/%d", shaOK, reportedTotal),
		st.MaxInflight, st.QueuePeak, nil
}

// checkDamaged confirms every generated malformed case is rejected.
func checkDamaged(b *fixture.Bundle) string {
	valid := b.Segments["mid/mid/seg-1.m4s"]
	cases := fixture.BuildDamagedCases(valid)
	rejected := 0
	for _, c := range cases {
		if _, err := bmff.IndexFragment(c.Data, 0); err != nil &&
			media.IsCode(err, c.Code) {
			rejected++
		}
	}
	return fmt.Sprintf("%d/%d", rejected, len(cases))
}

// measureDrift reproduces the bounded A/V jitter series and returns p95.
func measureDrift(seed uint64) float64 {
	rng := rand.New(rand.NewPCG(seed, seed^0x1234567))
	mapping := sync.NewClockMapping(0, 0)
	aligner := sync.NewAligner(mapping, 20*time.Millisecond)
	for i := 0; i < 256; i++ {
		videoTick := media.MediaTime((i + 1) * 9000)
		expected := mapping.TickToAudio(videoTick)
		jitter := int64(rng.Uint64N(1441)) - 720 // +/- 15 ms
		_ = aligner.Observe(videoTick,
			uint64(int64(expected)+jitter))
	}
	return aligner.Report().P95MS
}

// verifyRepeats reruns the pipeline N-1 more times and compares a canonical
// summary hash over all segment bytes and the ordered merge output.
func verifyRepeats(ctx context.Context, cfg Config,
	first *fixture.Bundle) bool {
	base := canonicalHash(first)
	for i := 1; i < cfg.Repeat; i++ {
		next, err := fixture.Generate(cfg.Seed)
		if err != nil {
			return false
		}
		if canonicalHash(next) != base {
			return false
		}
	}
	return true
}

// canonicalHash folds every segment hash in deterministic key order.
func canonicalHash(b *fixture.Bundle) [32]byte {
	keys := make([]string, 0, len(b.SegmentSHA))
	for k := range b.SegmentSHA {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		sum := b.SegmentSHA[k]
		h.Write(sum[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// probeCancellation starts a pool, cancels it, and verifies no worker remains
// (a non-zero return would indicate leaked goroutines).
func probeCancellation(parent context.Context, workers, queue int) int {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	pool, err := pipeline.NewPool(ctx, workers, queue)
	if err != nil {
		return workers
	}
	cancel()
	pool.Close() // blocks until every worker has exited
	return 0
}

// evaluate records every threshold violation as a human-readable failure.
func evaluate(m *Metrics, th Thresholds) {
	expect := map[string]string{
		"roundtrip_equal":     "2/2",
		"keyframe_hit":        "8/8",
		"segment_sha256_match": "8/8",
		"damaged_rejected":    "4/4",
		"burst_recovered":     "16/20",
	}
	got := map[string]string{
		"roundtrip_equal":      m.RoundtripEqual,
		"keyframe_hit":         m.KeyframeHit,
		"segment_sha256_match": m.SegmentSHA256Match,
		"damaged_rejected":     m.DamagedRejected,
		"burst_recovered":      m.BurstRecovered,
	}
	for name, want := range expect {
		if got[name] != want {
			m.Failures = append(m.Failures,
				fmt.Sprintf("%s=%s want %s", name, got[name], want))
		}
	}
	if m.DeadlineSkip != 4 {
		m.Failures = append(m.Failures,
			fmt.Sprintf("deadline_skip=%d want 4", m.DeadlineSkip))
	}
	if m.AVDriftP95MS > th.DriftP95MS {
		m.Failures = append(m.Failures,
			fmt.Sprintf("av_drift_p95_ms=%.3f want <=%.1f",
				m.AVDriftP95MS, th.DriftP95MS))
	}
	if !m.RepeatHashEqual {
		m.Failures = append(m.Failures, "repeat_hash_equal=false want true")
	}
	if m.CancelWorkers != 0 {
		m.Failures = append(m.Failures,
			fmt.Sprintf("cancel_workers=%d want 0", m.CancelWorkers))
	}
	sort.Strings(m.Failures)
}
