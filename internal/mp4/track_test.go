package mp4

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"medpipe/internal/clock"
)

// ffmpegAvailable reports whether the command line decoder is installed.
func ffmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// makeMP4 renders a short clip with explicit channel count, sample rate and
// color tags. Tests skip when ffmpeg is absent.
func makeMP4(t *testing.T, dir, name string, channels int, rate int, colorTags []string) string {
	t.Helper()
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, name)
	args := []string{
		"-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-ac", itoa(channels), "-ar", itoa(rate),
		"-movflags", "+faststart",
	}
	args = append(args, colorTags...)
	args = append(args, out)
	cmd := exec.Command("ffmpeg", args...)
	if outb, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg failed: %v\n%s", err, outb)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func TestParseStereo48WithColor(t *testing.T) {
	dir := t.TempDir()
	path := makeMP4(t, dir, "stereo48.mp4", 2, 48000,
		[]string{"-color_primaries", "bt709", "-color_trc", "bt709",
			"-colorspace", "bt709", "-movflags", "+write_colr"})
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := Parse(f, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(c.Tracks))
	}
	var video, audio *Track
	for _, tr := range c.Tracks {
		switch tr.Kind {
		case KindVideo:
			video = tr
		case KindAudio:
			audio = tr
		}
	}
	if video == nil || audio == nil {
		t.Fatalf("missing roles: %+v", c.Tracks)
	}
	if video.Width != 320 || video.Height != 240 || len(video.Samples) == 0 {
		t.Fatalf("video metadata wrong: %+v", video)
	}
	if !video.Samples[0].IsSync {
		t.Fatal("first video sample must be a sync sample")
	}
	if audio.Channels != 2 || audio.SampleRate != 48000 {
		t.Fatalf("audio metadata wrong: ch=%d rate=%d", audio.Channels, audio.SampleRate)
	}
	if len(audio.Samples) == 0 || audio.Samples[0].ByteOffset <= 0 {
		t.Fatalf("audio sample table not expanded: %+v", audio.Samples[:1])
	}
	// ffmpeg tags bt709 as primaries=2 (unspecified->709 mapping), trc=2,
	// matrix=1 in the nclx colr box.
	if video.Color == nil || video.Color.ColorType != "nclx" ||
		video.Color.TransferCharacteristics != 2 || video.Color.MatrixCoefficients != 1 {
		t.Fatalf("bt709 color metadata not preserved: %+v", video.Color)
	}
}

func TestParseMultiChannel441(t *testing.T) {
	dir := t.TempDir()
	path := makeMP4(t, dir, "multi441.mov", 6, 44100, nil)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := Parse(f, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range c.Tracks {
		if tr.Kind == KindAudio {
			if tr.Channels != 6 || tr.SampleRate != 44100 {
				t.Fatalf("want 6ch/44100, got %d/%d", tr.Channels, tr.SampleRate)
			}
			return
		}
	}
	t.Fatal("no audio track")
}

func TestParseTruncatedBox(t *testing.T) {
	dir := t.TempDir()
	path := makeMP4(t, dir, "cut.mp4", 2, 48000, nil)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := append([]byte(nil), raw[:len(raw)-5]...)
	bad := filepath.Join(dir, "bad.mp4")
	if err := os.WriteFile(bad, cut, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = Parse(f, DefaultLimits)
	var pe *clock.Error
	if err == nil {
		t.Fatal("truncated file parsed without error")
	}
	if !errors.As(err, &pe) || pe.Kind != clock.KindCorrupt {
		t.Fatalf("want corrupt, got %v", err)
	}
	if pe.Offset < 0 {
		t.Fatalf("corruption offset missing: %v", pe)
	}
}

func TestParseIllegalLength(t *testing.T) {
	dir := t.TempDir()
	path := makeMP4(t, dir, "x.mp4", 1, 48000, nil)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the first top-level box length.
	raw[0], raw[1], raw[2], raw[3] = 0, 0, 0, 3
	bad := filepath.Join(dir, "badlen.mp4")
	if err := os.WriteFile(bad, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = Parse(f, DefaultLimits)
	var pe2 *clock.Error
	if !clock.IsKind(err, clock.KindCorrupt) || !errors.As(err, &pe2) || pe2.Offset != 0 {
		t.Fatalf("want corrupt at byte 0, got %v", err)
	}
}
