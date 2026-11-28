package segment

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"presspack/internal/adapter"
	"presspack/internal/core"
	"presspack/internal/ingest"
)

func makePlan(t *testing.T, seconds string) *ingest.CameraPlan {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	src := filepath.Join(t.TempDir(), "cam.mp4")
	out, err := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration="+seconds,
		"-c:v", "libx264", "-g", "25", "-pix_fmt", "yuv420p", src).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	plan, err := ingest.BuildPlan(context.Background(), adapter.FFprobeV1{},
		"cam1", src, core.FromMillis(2000))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestEncodeTwoVariantsAndResume(t *testing.T) {
	plan := makePlan(t, "2")
	out := t.TempDir()
	variants := VariantsFromTargets([]int{400000, 800000})
	seg := core.FromMillis(2000)

	res, err := EncodeCamera(context.Background(), plan, out, variants, seg, "sha-src")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Segments != 1 {
		t.Fatalf("results = %+v", res)
	}
	for _, r := range res {
		if r.Resumed {
			t.Error("first run must encode, not resume")
		}
		if _, err := os.Stat(r.PlaylistPath); err != nil {
			t.Errorf("missing playlist: %v", err)
		}
		if _, err := os.Stat(filepath.Join(r.Dir, "init.mp4")); err != nil {
			t.Errorf("missing init segment: %v", err)
		}
	}
	// Rerun: identical parameters must resume without re-segmenting.
	res2, err := EncodeCamera(context.Background(), plan, out, variants, seg, "sha-src")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res2 {
		if !r.Resumed {
			t.Errorf("second run of %s must resume", r.Variant.Name)
		}
	}
	// Changed source hash invalidates the cache.
	res3, _ := EncodeCamera(context.Background(), plan, out, variants, seg, "sha-other")
	for _, r := range res3 {
		if r.Resumed {
			t.Errorf("changed source must re-encode %s", r.Variant.Name)
		}
	}
}

func TestCheckAlignment(t *testing.T) {
	frame := core.Rat(1, 25)
	ft := core.FromUnits(1, frame)
	starts := map[string][]core.Time{
		"v400": {core.Time{}, core.FromMillis(2000), core.FromMillis(4000)},
		"v800": {core.Time{}, core.FromMillis(2000), core.FromMillis(4000).Add(core.FromMillis(10))},
	}
	rep, faults := CheckAlignment(starts, ft)
	if len(faults) != 0 {
		t.Errorf("faults = %v", faults)
	}
	if rep.MaxError.Millis() != 10 || !rep.WithinFrame {
		t.Errorf("report = %+v", rep)
	}
	starts["v800"][2] = starts["v800"][2].Add(core.FromMillis(100))
	rep, _ = CheckAlignment(starts, ft)
	if rep.WithinFrame {
		t.Error("110 ms divergence at 25 fps must exceed one frame")
	}
}

func TestCheckSubtitleFit(t *testing.T) {
	frame := core.FromUnits(1, core.Rat(1, 25))
	d, ok := CheckSubtitleFit(core.FromMillis(2010), core.FromMillis(2000), frame)
	if !ok || d.Millis() != 10 {
		t.Errorf("fit = %v, %v", d, ok)
	}
	if _, ok := CheckSubtitleFit(core.FromMillis(2500), core.FromMillis(2000), frame); ok {
		t.Error("500 ms overrun must not fit")
	}
}
