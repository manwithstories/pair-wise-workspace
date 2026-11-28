package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"avpipe/internal/audit"
	"avpipe/internal/clock"
	"avpipe/internal/fixture"
	"avpipe/internal/media"
)

// generateFixtures wraps fixture.GenerateAll for the CLI.
func generateFixtures(dir string, seed int64) (fixture.Set, error) {
	return fixture.GenerateAll(dir, seed)
}

// roundtripRate reports the fraction of source encoded packets, counted with
// multiplicity, whose payload bytes appear in the output at least as many
// times as in the source. It is 1.0 exactly when the lossless remux preserved
// every packet's bytes and track association, and it can never exceed 1.0.
func roundtripRate(srcPath, outPath string) float64 {
	src, err := payloadHashes(srcPath)
	if err != nil {
		return 0
	}
	out, err := payloadHashes(outPath)
	if err != nil {
		return 0
	}
	total, matched := 0, 0
	for h, n := range src {
		total += n
		if m := out[h]; m > 0 {
			if m >= n {
				matched += n
			} else {
				matched += m
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(matched) / float64(total)
}

func payloadHashes(path string) (map[string]int, error) {
	pkts, err := readPackets(path)
	if err != nil {
		return nil, err
	}
	m := map[string]int{}
	for _, p := range pkts {
		m[p.PayloadHash()]++
	}
	return m, nil
}

// keyframeHitRate reports the fraction of segments that start on a keyframe.
func keyframeHitRate(manifestPath string) float64 {
	m, err := audit.Load(manifestPath)
	if err != nil || len(m.Segments) == 0 {
		return 0
	}
	ok := 0
	for _, s := range m.Segments {
		if s.FirstKey {
			ok++
		}
	}
	return float64(ok) / float64(len(m.Segments))
}

func segmentHashMatch(aPath, bPath string) float64 {
	a, err := audit.Load(aPath)
	if err != nil {
		return 0
	}
	b, err := audit.Load(bPath)
	if err != nil || len(a.Segments) == 0 {
		return 0
	}
	bByIndex := map[int]string{}
	for _, s := range b.Segments {
		bByIndex[s.Index] = s.Hash
	}
	ok := 0
	for _, s := range a.Segments {
		if bByIndex[s.Index] == s.Hash {
			ok++
		}
	}
	return float64(ok) / float64(len(a.Segments))
}

func replayEqual(aPath, bPath string) (bool, string) {
	a, err := audit.Load(aPath)
	if err != nil {
		return false, err.Error()
	}
	b, err := audit.Load(bPath)
	if err != nil {
		return false, err.Error()
	}
	return audit.CompareRuns(a, b)
}

func countVFRFromManifest(manifestPath string) int {
	m, err := audit.Load(manifestPath)
	if err != nil {
		return 0
	}
	return m.VFRFrames
}

// countDriftReasons returns how many segments recorded a clock-drift cut.
func countDriftReasons(manifestPath string) int {
	m, err := audit.Load(manifestPath)
	if err != nil {
		return 0
	}
	n := 0
	for _, s := range m.Segments {
		for _, r := range s.Reasons {
			if r == clock.DriftExceeded.String() || r == clock.DriftJump.String() {
				n++
				break
			}
		}
	}
	return n
}

// expectRejection runs a conversion that must fail with a corrupt verdict and
// must not publish output.
func expectRejection(ctx context.Context, path, dir string, cfg selfcheckConfig) (bool, error) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	out := filepath.Join(dir, "reject-"+base+".mp4")
	manifest := filepath.Join(dir, "reject-"+base+".json")
	_, err := run(ctx, runConfig{
		Inputs: []string{path}, Output: out, Manifest: manifest, Format: "mp4",
		Segment: cfg.Segment, Workers: cfg.Workers, Queue: cfg.Queue, Cache: cfg.Cache,
		WrapBits: cfg.WrapBits,
	})
	if err == nil {
		return false, nil
	}
	if media.KindOf(err) != media.KindCorrupt && media.KindOf(err) != media.KindPolicy {
		return false, err
	}
	// Ensure nothing was published.
	if _, statErr := os.Stat(out); statErr == nil {
		return false, nil
	}
	return true, nil
}

// measureCancel cancels a conversion mid-stream and returns how many
// milliseconds elapsed before the pipeline returned, plus whether it left the
// output directory free of published or temporary files. The cancel budget is
// reached after a handful of packets, which exercises the feeder-to-worker-to-
// muxer propagation rather than a context that was already done.
func measureCancel(ctx context.Context, input, dir string, cfg selfcheckConfig) (ms int64, clean bool) {
	if input == "" {
		input = filepath.Join(dir, "fixtures", "cancel.mp4")
	}
	out := filepath.Join(dir, "cancel-out.mp4")
	manifest := filepath.Join(dir, "cancel-out.json")
	start := time.Now()
	_, err := run(ctx, runConfig{
		Inputs: []string{input}, Output: out, Manifest: manifest, Format: "mp4",
		Segment: cfg.Segment, Workers: cfg.Workers, Queue: cfg.Queue, Cache: cfg.Cache,
		WrapBits: cfg.WrapBits, CancelAfter: 8,
	})
	elapsed := time.Since(start)
	// The run must have come back as a cancellation.
	if err == nil || media.KindOf(err) != media.KindCancel {
		return 5000, false
	}
	clean = true
	if _, statErr := os.Stat(out); statErr == nil {
		clean = false
	}
	if _, statErr := os.Stat(manifest); statErr == nil {
		clean = false
	}
	// No temporary files may survive under a name derived from the output.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "cancel-out.mp4.tmp-") ||
			strings.Contains(e.Name(), "cancel-out.json.tmp-") {
			clean = false
		}
	}
	if elapsed.Milliseconds() > 5000 {
		return 5000, clean
	}
	return elapsed.Milliseconds(), clean
}

// wrapRecoveryCount converts the wrap fixture and reports how many wraps the
// demuxer expanded, taken from the manifest.
func wrapRecoveryCount(ctx context.Context, wrapPath, dir string, cfg selfcheckConfig) int {
	if wrapPath == "" {
		return 0
	}
	out := filepath.Join(dir, "wrap-out.mp4")
	manifest := filepath.Join(dir, "wrap-manifest.json")
	if _, err := run(ctx, runConfig{
		Inputs: []string{wrapPath}, Output: out, Manifest: manifest, Format: "mp4",
		Segment: cfg.Segment, Workers: cfg.Workers, Queue: cfg.Queue, Cache: cfg.Cache,
		WrapBits: 33, AllowQuarantine: true,
	}); err != nil {
		return 0
	}
	m, err := audit.Load(manifest)
	if err != nil {
		return 0
	}
	return m.WrapRecovered
}

func describeShortfalls(r *selfcheckReport, cfg selfcheckConfig) []string {
	var out []string
	if r.RoundtripRate < 1 {
		out = append(out, "roundtrip_rate < 1")
	}
	if r.KeyframeHitRate < 1 {
		out = append(out, "keyframe_hit_rate < 1")
	}
	if r.SegmentHashMatch < 1 {
		out = append(out, "segment_hash_match < 1")
	}
	if r.Segments != 6 {
		out = append(out, fmt.Sprintf("segments=%d want 6", r.Segments))
	}
	if r.WrapRecovered != 2 {
		out = append(out, fmt.Sprintf("wrap_recovered=%d want 2", r.WrapRecovered))
	}
	if r.VFRFrames != 120 {
		out = append(out, fmt.Sprintf("vfr_frames=%d want 120", r.VFRFrames))
	}
	if r.CorruptRejected < 4 {
		out = append(out, fmt.Sprintf("corrupt_rejected=%d want >=4", r.CorruptRejected))
	}
	if r.ReplayEqual != 1 {
		out = append(out, "replay_equal != 1")
	}
	if r.QueuePeak > cfg.Queue {
		out = append(out, fmt.Sprintf("queue_peak=%d exceeds %d", r.QueuePeak, cfg.Queue))
	}
	if r.CancelMS > 100 {
		out = append(out, fmt.Sprintf("cancel_ms=%d exceeds 100", r.CancelMS))
	}
	if !r.CancelClean {
		out = append(out, "canceled run published output or left temp files")
	}
	if r.DriftCuts == 0 {
		out = append(out, "drift fixture recorded no drift cut")
	}
	sort.Strings(out)
	return out
}
