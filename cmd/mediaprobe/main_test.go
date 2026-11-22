package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"mediaprobe/internal/fixture"
)

func demoResult(t *testing.T) *pipeResult {
	t.Helper()
	res, err := runPipeline(fixture.Options{
		Seed: 42, Fragments: 12, DriftPPM: 120, WrapAt: 1 << 33, VFR: true,
	}, 8, 1<<20, time.Millisecond)
	if err != nil {
		t.Fatalf("runPipeline: %v", err)
	}
	return res
}

func TestDemoMetrics(t *testing.T) {
	res := demoResult(t)
	s := res.summary
	if s.RoundtripPct < 99.0 || s.RoundtripPct > 100.0 {
		t.Errorf("roundtrip=%.1f", s.RoundtripPct)
	}
	if s.KeyframeHitPct < 95.0 || s.KeyframeHitPct > 100.0 {
		t.Errorf("keyframe hit=%.1f", s.KeyframeHitPct)
	}
	if len(s.Segments) == 0 || s.UniqueHashes() != len(s.Segments) {
		t.Errorf("segments=%d unique=%d", len(s.Segments), s.UniqueHashes())
	}
	if s.QueuePeak > 8 {
		t.Errorf("queue_peak=%d exceeds capacity", s.QueuePeak)
	}
	if s.DriftPPM < 119 || s.DriftPPM > 121 {
		t.Errorf("drift_ppm=%.1f", s.DriftPPM)
	}
	if s.SilenceIntervals == 0 {
		t.Error("expected silence intervals")
	}
	if s.TFDTRegressions != 0 || s.CorruptFragments != 0 {
		t.Errorf("unexpected faults: reg=%d corrupt=%d", s.TFDTRegressions, s.CorruptFragments)
	}
}

func TestRerunProducesIdenticalHashes(t *testing.T) {
	a, b := demoResult(t), demoResult(t)
	if len(a.summary.Segments) != len(b.summary.Segments) {
		t.Fatal("segment count differs across runs")
	}
	for i := range a.summary.Segments {
		if a.summary.Segments[i].Hash != b.summary.Segments[i].Hash {
			t.Fatalf("segment %d hash differs across runs", i)
		}
	}
}

// TestFaultsPropagateAcrossStages injects faults of two classes (structural
// corruption and timeline/sequence faults) and asserts the invariants that
// must hold after recovery at the parse, timeline, cut and dedup stages.
func TestFaultsPropagateAcrossStages(t *testing.T) {
	cases := []struct {
		mode   fixture.CorruptMode
		assert func(t *testing.T, r *pipeResult)
	}{
		{fixture.CorruptTruncatedBox, func(t *testing.T, r *pipeResult) {
			if r.firstCorrupt == nil || r.firstCorrupt.Offset <= 0 {
				t.Error("truncated box not isolated with offset")
			}
			if r.summary.CorruptFragments != 1 {
				t.Errorf("corrupt=%d", r.summary.CorruptFragments)
			}
			// Invariant: later fragments still cut into segments.
			if r.recoveredSegments == 0 {
				t.Error("no segments recovered after isolation")
			}
		}},
		{fixture.CorruptSeqGap, func(t *testing.T, r *pipeResult) {
			if r.summary.SeqGaps == 0 {
				t.Error("sequence gap not detected")
			}
			// Invariant: missing numbers never invent samples; the audio
			// gap surfaces as an unknown interval with a reason.
			if r.summary.UnknownIntervals == 0 {
				t.Error("gap did not produce an unknown audio interval")
			}
		}},
		{fixture.CorruptTFDTRegression, func(t *testing.T, r *pipeResult) {
			if r.summary.TFDTRegressions != 1 || r.summary.UncertainIntervals == 0 {
				t.Errorf("reg=%d unc=%d", r.summary.TFDTRegressions, r.summary.UncertainIntervals)
			}
		}},
		{fixture.CorruptDuplicate, func(t *testing.T, r *pipeResult) {
			if r.summary.DuplicateSegments == 0 {
				t.Error("cross-fragment duplicate not detected")
			}
			if r.summary.UniqueHashes() == len(r.summary.Segments) {
				t.Error("duplicate did not reduce unique hash count")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			res, err := runPipeline(fixture.Options{Seed: 42, Fragments: 12, VFR: true, Corrupt: tc.mode}, 8, 1<<20, 0)
			if err != nil {
				t.Fatal(err)
			}
			tc.assert(t, res)
			// Invariant: recovery is deterministic — rerun yields the
			// same segment hashes.
			again, err := runPipeline(fixture.Options{Seed: 42, Fragments: 12, VFR: true, Corrupt: tc.mode}, 8, 1<<20, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.summary.Segments) != len(again.summary.Segments) {
				t.Fatal("rerun segment count differs")
			}
			for i := range res.summary.Segments {
				if res.summary.Segments[i].Hash != again.summary.Segments[i].Hash {
					t.Fatalf("rerun hash differs at segment %d", i)
				}
			}
		})
	}
}

func TestExitCodes(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(nil, &out, &errBuf); code != exitUsage {
		t.Errorf("no args: %d", code)
	}
	out.Reset()
	if code := run([]string{"demo", "--seed", "42", "--queue", "8", "--drift-ppm", "120", "--wrap-at", "8589934592"}, &out, &errBuf); code != exitOK {
		t.Errorf("demo: %d (%s)", code, errBuf.String())
	}
	for _, key := range []string{"roundtrip_consistency_pct=", "keyframe_index_hit_pct=", "segments=", "segment_sha256_count=", "queue_peak=", "drift_ppm=120", "segment[0].sha256="} {
		if !strings.Contains(out.String(), key) {
			t.Errorf("demo output missing %q", key)
		}
	}
	out.Reset()
	if code := run([]string{"verify", "--seed", "42", "--corrupt", "truncated-box"}, &out, &errBuf); code != exitCorrupt {
		t.Errorf("verify truncated-box: %d", code)
	}
	for _, key := range []string{"bad_box_offset=", "recovered_segments=", "exit_code=2"} {
		if !strings.Contains(out.String(), key) {
			t.Errorf("verify output missing %q", key)
		}
	}
	out.Reset()
	if code := run([]string{"verify", "--seed", "42", "--corrupt", "bogus"}, &out, &errBuf); code != exitUsage {
		t.Errorf("bad mode: %d", code)
	}
}
