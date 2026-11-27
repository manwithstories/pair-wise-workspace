package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
)

// fixtureSpec describes one generated rendition.
type fixtureSpec struct {
	name      string
	size      string
	layout    string // "6" for 5.1, "2" for stereo
	rate      int
	colorArgs []string
	vbps      string
}

// cmdFixture deterministically generates a two-rendition delivery package
// with subtitles, chapters and timed metadata events, plus a truncated
// variant for negative testing.
func cmdFixture(args []string) error {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	seed := fs.Int64("seed", 1, "deterministic seed")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("--out is required")
	}
	rng := rand.New(rand.NewSource(*seed))
	good := filepath.Join(*out, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		return err
	}
	const durSec = 6
	specs := []fixtureSpec{
		{name: "rend_hi.mp4", size: "1280x720", layout: "6", rate: 48000,
			colorArgs: []string{"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709"},
			vbps: "1500k"},
		{name: "rend_lo.mp4", size: "640x360", layout: "2", rate: 44100,
			colorArgs: []string{"-colorspace", "smpte170m", "-color_primaries", "smpte170m", "-color_trc", "smpte170m"},
			vbps: "600k"},
	}
	subPath, err := writeSubtitles(good, rng, durSec)
	if err != nil {
		return err
	}
	metaPath, err := writeChapters(good, rng, durSec)
	if err != nil {
		return err
	}
	for _, s := range specs {
		if err := genRendition(good, s, subPath, metaPath, durSec, rng); err != nil {
			return err
		}
	}
	if err := writeEvents(good, rng, durSec); err != nil {
		return err
	}
	if err := writeDelivery(good, specs); err != nil {
		return err
	}
	if err := makeTruncated(*out, good, specs[0].name); err != nil {
		return err
	}
	printJSON(map[string]any{
		"seed": *seed, "good": good, "truncated": filepath.Join(*out, "truncated"),
		"duration_s": durSec, "renditions": len(specs),
	})
	return nil
}

// genRendition renders one mp4 with video, audio, subtitles and chapters.
func genRendition(dir string, s fixtureSpec, subPath, metaPath string, durSec int, rng *rand.Rand) error {
	freq := 380 + rng.Intn(200)
	out := filepath.Join(dir, s.name)
	args := []string{"-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=25:duration=%d", s.size, durSec),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=%d:duration=%d", freq, s.rate, durSec),
		"-i", subPath,
		"-f", "ffmetadata", "-i", metaPath,
		"-map", "0:v", "-map", "1:a", "-map", "2:s", "-map_metadata", "3",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-pix_fmt", "yuv420p",
		"-b:v", s.vbps, "-minrate", s.vbps, "-maxrate", s.vbps, "-bufsize", "1M",
		"-c:a", "aac", "-b:a", "128k", "-ac", s.layout,
		"-c:s", "mov_text", "-movflags", "+faststart"}
	args = append(args, s.colorArgs...)
	args = append(args, out)
	cmd := exec.Command("ffmpeg", args...)
	if out2, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fixture rendition %s: %v: %s", s.name, err, out2)
	}
	return nil
}

// makeTruncated copies the package and cuts the primary rendition short.
func makeTruncated(root, good, victim string) error {
	tr := filepath.Join(root, "truncated")
	if err := os.MkdirAll(tr, 0o755); err != nil {
		return err
	}
	for _, f := range []string{"delivery.json", "events.json", victim} {
		raw, err := os.ReadFile(filepath.Join(good, f))
		if err != nil {
			return err
		}
		if f == victim {
			raw = raw[:len(raw)*55/100]
		}
		if err := os.WriteFile(filepath.Join(tr, f), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}
