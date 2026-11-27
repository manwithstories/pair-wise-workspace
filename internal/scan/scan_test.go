package scan

import (
	"os"
	"path/filepath"
	"testing"

	"aircheck/internal/fault"
)

// makeMedia builds a 2 s test file with video, audio and subtitles.
func makeMedia(t *testing.T, dir, name string) string {
	t.Helper()
	srt := filepath.Join(dir, name+".srt")
	if err := os.WriteFile(srt, []byte(
		"1\n00:00:00,200 --> 00:00:01,000\nhello\n\n2\n00:00:01,200 --> 00:00:01,800\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	if _, err := execOutput("ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2:sample_rate=48000",
		"-i", srt,
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-c:s", "mov_text", "-movflags", "+faststart", out); err != nil {
		t.Fatalf("fixture media: %v", err)
	}
	return out
}

func deliveryFor(file string) *Delivery {
	d := &Delivery{Package: "t", SegmentMS: 1000}
	d.Renditions = append(d.Renditions, struct {
		Name      string `json:"name"`
		File      string `json:"file"`
		TargetBPS int64  `json:"target_bps"`
	}{Name: "r0", File: file, TargetBPS: 500000})
	return d
}

func TestScanPackageGood(t *testing.T) {
	dir := t.TempDir()
	makeMedia(t, dir, "r0.mp4")
	res, err := ScanPackage(dir, deliveryFor("r0.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Renditions) != 1 || len(res.Renditions[0].Tracks) < 3 {
		t.Fatalf("tracks missing: %+v", res.Renditions)
	}
	var subs int
	for _, e := range res.Events {
		if e.Type == "subtitle" {
			subs++
		}
	}
	if subs != 2 {
		t.Fatalf("want 2 subtitle events, got %d", subs)
	}
	if res.Renditions[0].DurationTicks != 180000 {
		t.Fatalf("duration ticks = %d", res.Renditions[0].DurationTicks)
	}
}

func TestScanTruncated(t *testing.T) {
	dir := t.TempDir()
	src := makeMedia(t, dir, "r0.mp4")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	trunc := filepath.Join(dir, "tr.mp4")
	if err := os.WriteFile(trunc, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ScanPackage(dir, deliveryFor("tr.mp4"))
	fe := fault.Of(err)
	if fe == nil || fe.Code != fault.CodeTruncated || fe.Track == "" || fe.Tick < 0 {
		t.Fatalf("want truncation fault with track+tick, got %+v", fe)
	}
}

func TestSidecarEventPastEnd(t *testing.T) {
	dir := t.TempDir()
	makeMedia(t, dir, "r0.mp4")
	if err := os.WriteFile(filepath.Join(dir, "events.json"),
		[]byte(`{"events":[{"type":"id3","time_ms":99000,"payload":"x"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ScanPackage(dir, deliveryFor("r0.mp4"))
	if fe := fault.Of(err); fe == nil || fe.Code != fault.CodeMalformedEvent || fe.Tick <= 0 {
		t.Fatalf("want malformed event fault, got %+v", fe)
	}
}

func TestParseSRT(t *testing.T) {
	ev, err := parseSRT("1\n00:00:01,000 --> 00:00:02,500\nhi\n", "t", 200000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Tick != 90000 || ev[0].DurTicks != 135000 {
		t.Fatalf("bad cue: %+v", ev)
	}
	if _, err := parseSRT("1\n00:00:02,000 --> 00:00:01,000\nneg\n", "t", 200000); fault.Of(err) == nil {
		t.Fatal("negative cue duration must fault")
	}
}
