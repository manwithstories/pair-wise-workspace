package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"mediamux/media"
	"mediamux/testdata"
)

func writeTempMP4(t *testing.T, sizes []uint32) string {
	t.Helper()
	data, err := testdata.MP4(sizes)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sample.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadMP4Roundtrip(t *testing.T) {
	// 100 帧 @25fps，每帧 12000 字节 → 4s，2.4Mbps
	sizes := make([]uint32, 100)
	for i := range sizes {
		sizes[i] = 12000
	}
	tracks, err := ReadMP4(writeTempMP4(t, sizes))
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 {
		t.Fatalf("tracks = %d", len(tracks))
	}
	tr := tracks[0]
	if tr.Codec != "avc1" || tr.Timescale != 90000 || tr.Samples != 100 {
		t.Fatalf("track = %+v", tr)
	}
	if ms := media.Millis(tr.Duration); ms != 4000 {
		t.Fatalf("duration = %d ms", ms)
	}
	if tr.Bitrate != 2400000 {
		t.Fatalf("bitrate = %d", tr.Bitrate)
	}
}

func TestReadMP4Missing(t *testing.T) {
	if _, err := ReadMP4(filepath.Join(t.TempDir(), "none.mp4")); err == nil {
		t.Fatal("missing file should fail")
	} else if media.IsRecoverable(err) {
		t.Fatal("missing file should be unrecoverable")
	}
}

func TestValidate(t *testing.T) {
	frameDur := media.FrameRate25.FrameDuration()
	m := &Manifest{
		Tracks: []Track{{ID: 1, Codec: "avc1", Duration: media.MillisToTicks(4000)}},
		Subtitles: []SubRange{
			{Start: media.MillisToTicks(0), End: media.MillisToTicks(1600)},
			{Start: media.MillisToTicks(2000), End: media.MillisToTicks(3600)},
		},
		TargetBps: 2400000,
		ActualBps: 2380000,
	}
	checks := m.Validate(frameDur)
	if !m.AllOK() {
		t.Fatalf("checks = %+v", checks)
	}
	if got := m.AlignmentError(); got != 0 {
		t.Fatalf("align err = %d", got)
	}

	// 字幕超出轨道覆盖：对齐误差 = 超出量
	m.Subtitles = append(m.Subtitles, SubRange{
		Start: media.MillisToTicks(4000), End: media.MillisToTicks(4100),
	})
	m.Validate(frameDur)
	if m.AllOK() {
		t.Fatal("overflow subtitle should fail alignment")
	}
	if got := media.Millis(m.AlignmentError()); got != 100 {
		t.Fatalf("align err = %d ms", got)
	}

	// 时间倒退：单调性失败
	m2 := &Manifest{
		Tracks: []Track{{ID: 1, Duration: media.MillisToTicks(4000)}},
		Subtitles: []SubRange{
			{Start: media.MillisToTicks(2000), End: media.MillisToTicks(3000)},
			{Start: media.MillisToTicks(1000), End: media.MillisToTicks(1500)},
		},
	}
	m2.Validate(frameDur)
	if m2.AllOK() {
		t.Fatal("non-monotonic subtitles should fail")
	}
}
