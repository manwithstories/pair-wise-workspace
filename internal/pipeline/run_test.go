package pipeline_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/fixture"
	"mediad/internal/manifest"
	"mediad/internal/pipeline"
)

func generate(t *testing.T) []fixture.Generated {
	t.Helper()
	cases, err := fixture.GenerateAll(t.TempDir(), 7, 48)
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

func loadManifest(t *testing.T, dir string) *manifest.Manifest {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := manifest.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func runCase(t *testing.T, dir string) *pipeline.Result {
	t.Helper()
	res, err := pipeline.Run(context.Background(), loadManifest(t, dir),
		pipeline.Options{Dir: dir, TargetBPS: 1800000})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func faultKinds(res *pipeline.Result) map[fault.Kind]int {
	out := map[fault.Kind]int{}
	for _, f := range res.Faults {
		out[f.Kind]++
	}
	return out
}

// TestCrossStage runs all three fixtures through the whole pipeline and
// checks counts, ranges, audio samples and recovery against the
// generator's expectations.
func TestCrossStage(t *testing.T) {
	for _, c := range generate(t) {
		res := runCase(t, c.Dir)
		if res.Frames != c.Expect.Frames {
			t.Errorf("%s: frames %d want %d", c.Name, res.Frames, c.Expect.Frames)
		}
		if len(res.CorruptRanges) != c.Expect.CorruptRanges {
			t.Errorf("%s: ranges %d want %d", c.Name, len(res.CorruptRanges), c.Expect.CorruptRanges)
		}
		if res.RecoveredFrames != c.Expect.RecoveredFrames {
			t.Errorf("%s: recovered %d want %d", c.Name, res.RecoveredFrames, c.Expect.RecoveredFrames)
		}
		for seq, want := range c.Expect.AudioSamples {
			if got := res.AudioSamples[seq]; got != want {
				t.Errorf("%s: audio %s = %d want %d", c.Name, seq, got, want)
			}
		}
		for _, r := range res.CorruptRanges {
			if !(r.Start < r.End) {
				t.Errorf("%s: range not half-open: %+v", c.Name, r)
			}
		}
		if res.AlignMaxMs != 0 {
			t.Errorf("%s: align %v want 0", c.Name, res.AlignMaxMs)
		}
		kinds := faultKinds(res)
		switch c.Name {
		case "success":
			if len(res.Faults) != 0 {
				t.Errorf("success: unexpected faults %v", res.Faults)
			}
			if math.Abs(res.DurationSec-2.0) > 1e-9 {
				t.Errorf("success: duration %v want 2.0", res.DurationSec)
			}
		case "corrupt":
			if kinds[fault.KindHashMismatch] != 1 || kinds[fault.KindTruncated] != 1 {
				t.Errorf("corrupt: kinds %v", kinds)
			}
		case "edge":
			if kinds[fault.KindBadTimestamp] != 1 || kinds[fault.KindUnsupportedLayout] != 1 ||
				kinds[fault.KindUnknownColor] != 1 {
				t.Errorf("edge: kinds %v", kinds)
			}
			var unrecoverable bool
			for _, f := range res.Faults {
				if !f.Recoverable {
					unrecoverable = true
				}
			}
			if !unrecoverable {
				t.Error("edge: expected an unrecoverable fault")
			}
		}
	}
}

// TestAlignErrorReported shifts one frame PTS by 90 ticks (1 ms at the
// 1/90000 time base) and checks the reported alignment error.
func TestAlignErrorReported(t *testing.T) {
	var dir string
	for _, c := range generate(t) {
		if c.Name == "success" {
			dir = c.Dir
		}
	}
	m := loadManifest(t, dir)
	m.Tracks[0].Frames[10].PTS += 90
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runCase(t, dir)
	if math.Abs(res.AlignMaxMs-1.0) > 1e-9 {
		t.Fatalf("align %v want 1.0 ms", res.AlignMaxMs)
	}
}

// TestCancelStopsRun verifies cancellation aborts the run.
func TestCancelStopsRun(t *testing.T) {
	var dir string
	for _, c := range generate(t) {
		if c.Name == "success" {
			dir = c.Dir
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pipeline.Run(ctx, loadManifest(t, dir), pipeline.Options{Dir: dir, TargetBPS: 1800000})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

// TestDeterministic reruns a case and requires identical stats.
func TestDeterministic(t *testing.T) {
	var dir string
	for _, c := range generate(t) {
		if c.Name == "corrupt" {
			dir = c.Dir
		}
	}
	a := runCase(t, dir)
	b := runCase(t, dir)
	if a.ProxyBytes != b.ProxyBytes || a.Frames != b.Frames ||
		a.AlignMaxMs != b.AlignMaxMs || len(a.Faults) != len(b.Faults) {
		t.Fatal("non-deterministic run")
	}
	for i := range a.FrameStats {
		if a.FrameStats[i] != b.FrameStats[i] {
			t.Fatalf("frame %d differs: %+v vs %+v", i, a.FrameStats[i], b.FrameStats[i])
		}
	}
}
