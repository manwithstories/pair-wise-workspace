package adapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"avcore/internal/clock"
)

func TestParseProbeSecondsInteger(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1.5", clock.TicksPerSecond + clock.TicksPerSecond/2},
		{"0.001", clock.TicksPerSecond / 1000},
		{"2.0", clock.TicksPerSecond * 2},
		{"123.456789012", clock.TicksPerSecond*123 + 456789012*clock.TicksPerSecond/1_000_000_000},
		{"-3.5", -int64(clock.TicksPerSecond*3 + clock.TicksPerSecond/2)},
	}
	for _, c := range cases {
		got, err := parseProbeSeconds(c.in)
		if err != nil {
			t.Errorf("%q: unexpected err %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q = %d ticks, want %d", c.in, got, c.want)
		}
	}
	if _, err := parseProbeSeconds("N/A"); err == nil {
		t.Error("N/A must be rejected")
	}
}

func TestParseRational(t *testing.T) {
	r, err := parseRational("30000/1001")
	if err != nil || r != clock.Rate2997 {
		t.Errorf("rational parse: %+v err=%v", r, err)
	}
	if _, err := parseRational("30/0"); err == nil {
		t.Error("zero denominator must fail")
	}
}

// makeSample renders a short real MP4 with ffmpeg for adapter integration.
func makeSample(t *testing.T, dir, rate string, seconds int) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	out := filepath.Join(dir, "sample.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate="+rate,
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", itoa(seconds), "-c:v", "libx264", "-preset", "ultrafast",
		"-g", "30", "-keyint_min", "30", "-c:a", "aac", "-y", out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg sample render: %v", err)
	}
	return out
}

func TestFFProbeV1Inspect(t *testing.T) {
	dir := t.TempDir()
	sample := makeSample(t, dir, "30", 2)
	p, err := NewFFProbeV1("")
	if err != nil {
		t.Skipf("ffprobe unavailable: %v", err)
	}
	info, err := p.Inspect(context.Background(), sample)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.Adapter != Version || info.Video == nil {
		t.Fatalf("bad info: %+v", info)
	}
	if info.Video.Width != 320 || info.Video.Height != 240 {
		t.Errorf("resolution = %dx%d", info.Video.Width, info.Video.Height)
	}
	if info.Video.FrameRate != clock.Rate30 {
		t.Errorf("frame rate = %+v, want 30/1", info.Video.FrameRate)
	}
	if info.Audio == nil || info.Audio.SampleRate != 48000 {
		t.Errorf("audio stream missing/wrong: %+v", info.Audio)
	}
	if info.DurationTicks < clock.TicksPerSecond || info.DurationTicks > clock.TicksPerSecond*3 {
		t.Errorf("duration out of band: %d ticks", info.DurationTicks)
	}
}

func TestFFProbeV1Keyframes(t *testing.T) {
	dir := t.TempDir()
	sample := makeSample(t, dir, "30", 3)
	p, err := NewFFProbeV1("")
	if err != nil {
		t.Skipf("ffprobe unavailable: %v", err)
	}
	kfs, err := p.Keyframes(context.Background(), sample)
	if err != nil {
		t.Fatalf("keyframes: %v", err)
	}
	if len(kfs) < 2 {
		t.Fatalf("expected multiple keyframes, got %d", len(kfs))
	}
	// Keyframes must be monotonically increasing and non-negative.
	for i := 1; i < len(kfs); i++ {
		if kfs[i].TimeTicks <= kfs[i-1].TimeTicks {
			t.Errorf("keyframe order broken at %d: %d <= %d", i, kfs[i].TimeTicks, kfs[i-1].TimeTicks)
		}
	}
}

func TestValidateInputRejectsUnsafe(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "ok.mp4")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mp4")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"", "https://example.com/a.mp4", "pipe:0",
		filepath.Join(dir, "..", "escape.mp4"), link,
	}
	for _, b := range bad {
		if err := ValidateInput(b); err == nil {
			t.Errorf("unsafe input accepted: %q", b)
		}
	}
	if err := ValidateInput(regular); err != nil {
		t.Errorf("regular input rejected: %v", err)
	}
}

func TestConfine(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "seg", "v0.m4s")
	if got, err := Confine(root, good); err != nil || got != good {
		t.Errorf("inside path: got %q err=%v", got, err)
	}
	if _, err := Confine(root, filepath.Join(root, "..", "evil.m4s")); err == nil {
		t.Error("escaping path must be rejected")
	}
}
