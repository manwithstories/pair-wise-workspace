package stream

import (
	"os"
	"path/filepath"
	"testing"

	"aircheck/internal/normalize"
	"aircheck/internal/plan"
	"aircheck/internal/scan"
)

// buildInput makes a 4 s two-rendition package on disk.
func buildInput(t *testing.T) (string, *scan.Result) {
	t.Helper()
	dir := t.TempDir()
	mk := func(name, layout string, rate int, colorArgs ...string) {
		out := filepath.Join(dir, name)
		args := []string{"-y", "-v", "error",
			"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=4",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=4:sample_rate=" + itoa(rate),
			"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-ac", "2", "-movflags", "+faststart"}
		args = append(args, colorArgs...)
		args = append(args, out)
		if _, err := execOutput("ffmpeg", args...); err != nil {
			t.Fatalf("mk input: %v", err)
		}
	}
	mk("r0.mp4", "stereo", 48000, "-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709")
	mk("r1.mp4", "stereo", 44100)

	del := &scan.Delivery{Package: "t", SegmentMS: 2000}
	del.Renditions = append(del.Renditions,
		struct {
			Name      string `json:"name"`
			File      string `json:"file"`
			TargetBPS int64  `json:"target_bps"`
		}{Name: "hi", File: "r0.mp4", TargetBPS: 800000},
		struct {
			Name      string `json:"name"`
			File      string `json:"file"`
			TargetBPS int64  `json:"target_bps"`
		}{Name: "lo", File: "r1.mp4", TargetBPS: 500000})
	res, err := scan.ScanPackage(dir, del)
	if err != nil {
		t.Fatal(err)
	}
	return dir, res
}

func itoa(i int) string { return string(rune('0'+i/10000%10)) + string(rune('0'+i/1000%10)) + string(rune('0'+i/100%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10)) }

func testOpts(out string) Options {
	return Options{OutDir: out, Resume: true,
		Audio: normalize.DefaultAudioPolicy(), Color: normalize.DefaultColorPolicy()}
}

func TestRunAllAndResume(t *testing.T) {
	inDir, res := buildInput(t)
	cp, err := plan.Build(res)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	notes, err := RunAll(res, cp, inDir, "key1", testOpts(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) == 0 {
		t.Fatal("expected normalization notes (44.1k resample, color retag)")
	}
	for _, r := range res.Renditions {
		segs, _ := os.ReadDir(filepath.Join(out, r.Name))
		var n int
		for _, s := range segs {
			if filepath.Ext(s.Name()) == ".ts" {
				n++
			}
		}
		if n != cp.NumSegments() {
			t.Fatalf("%s: %d segments, want %d", r.Name, n, cp.NumSegments())
		}
	}
	if _, err := os.Stat(filepath.Join(out, "master.m3u8")); err != nil {
		t.Fatal("master playlist missing")
	}

	// Corrupt one segment, then resume: it must be rebuilt, others reused.
	victim := filepath.Join(out, "hi", "seg00000.ts")
	if err := os.WriteFile(victim, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RunAll(res, cp, inDir, "key1", testOpts(out)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "corrupt" || len(raw) < 1000 {
		t.Fatal("corrupted segment not rebuilt on resume")
	}
}

func TestSegmentCountMismatchFaults(t *testing.T) {
	inDir, res := buildInput(t)
	cp, err := plan.Build(res)
	if err != nil {
		t.Fatal(err)
	}
	// Claim one more segment than ffmpeg will produce.
	cp.Cuts = append(cp.Cuts, cp.Cuts[len(cp.Cuts)-1]+cp.SegmentTicks)
	_, err = RunAll(res, cp, inDir, "k", testOpts(t.TempDir()))
	if err == nil {
		t.Fatal("want segment count fault")
	}
}
