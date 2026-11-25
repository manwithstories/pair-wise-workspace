package segment

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"medpipe/internal/mp4"
)

func ffmpegPresent(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// renderSource makes a 4 s clip with explicit color tags, 25 fps video and
// 48 kHz stereo AAC audio.
func renderSource(t *testing.T, dir string) string {
	t.Helper()
	if !ffmpegPresent(t) {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, "source.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-ar", "48000", "-ac", "2",
		"-color_primaries", "bt709", "-color_trc", "bt709",
		"-colorspace", "bt709", "-movflags", "+write_colr",
		out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render failed: %v\n%s", err, b)
	}
	return out
}

func TestWriterAtomicCutAndColor(t *testing.T) {
	dir := t.TempDir()
	source := renderSource(t, dir)
	f, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	c, err := mp4.Parse(f, mp4.DefaultLimits)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	planner, err := NewPlanner(c)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Build(1000)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	w, err := NewWriter(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range plan.Boundaries {
		path, err := w.Emit(context.Background(), source, c, b)
		if err != nil {
			t.Fatalf("emit segment %d: %v", b.Index, err)
		}
		if got := filepath.Base(path); got != segmentName(b.Index) {
			t.Fatalf("output name %s, want %s", got, segmentName(b.Index))
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			t.Fatalf("segment not committed: %v size=%v", err, info)
		}
	}
	// No temp files may remain after successful atomic commits.
	entries, _ := os.ReadDir(outDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	// Output segments must carry the source color metadata. Verify through
	// the project's own parser rather than ffprobe text formatting.
	seg, err := os.Open(filepath.Join(outDir, segmentName(0)))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mp4.Parse(seg, mp4.DefaultLimits)
	seg.Close()
	if err != nil {
		t.Fatal(err)
	}
	var srcColor, outColor *mp4.ColorMeta
	for _, tr := range c.Tracks {
		if tr.Kind == mp4.KindVideo {
			srcColor = tr.Color
		}
	}
	for _, tr := range parsed.Tracks {
		if tr.Kind == mp4.KindVideo {
			outColor = tr.Color
		}
	}
	if srcColor == nil {
		t.Fatal("source fixture has no color metadata to preserve")
	}
	if outColor == nil {
		t.Fatal("output segment lost the source colr box")
	}
	if outColor.ColourPrimaries != srcColor.ColourPrimaries ||
		outColor.TransferCharacteristics != srcColor.TransferCharacteristics ||
		outColor.MatrixCoefficients != srcColor.MatrixCoefficients {
		t.Fatalf("color metadata changed: source=%+v output=%+v", srcColor, outColor)
	}
}

func TestWriterInterruptionCleansTemp(t *testing.T) {
	dir := t.TempDir()
	source := renderSource(t, dir)
	f, _ := os.Open(source)
	c, err := mp4.Parse(f, mp4.DefaultLimits)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	w, _ := NewWriter(outDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // preemptive cancel simulates a write interruption
	_, err = w.Emit(ctx, source, c, Plan{Boundaries: []Boundary{{Index: 0,
		StartMillis: 0, EndMillis: 1000}}}.Boundaries[0])
	if err == nil {
		t.Fatal("interrupted write returned no error")
	}
	entries, _ := os.ReadDir(outDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("interrupted write left temp file: %s", e.Name())
		}
	}
}
