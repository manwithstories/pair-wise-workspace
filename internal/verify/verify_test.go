package verify

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"m3u8ctl/internal/fixture"
	"m3u8ctl/internal/gen"
	"m3u8ctl/internal/playlist"
	"m3u8ctl/internal/slicer"
	"m3u8ctl/internal/ts"
)

func buildHLS(t *testing.T, scenario string, renditions, segments, window int) string {
	t.Helper()
	root := t.TempDir()
	if err := fixture.Generate(fixture.Options{
		Out: root, Scenario: scenario, Renditions: renditions, Segments: segments, Seed: 7,
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, err := gen.Generate(gen.Config{
		Input: filepath.Join(root, "live"), Output: filepath.Join(root, "hls"),
		Window: window, TargetDuration: 2 * time.Second, EndList: true,
	}); err != nil {
		t.Fatalf("gen: %v", err)
	}
	return filepath.Join(root, "hls")
}

func verifyHLS(t *testing.T, root string) *Report {
	t.Helper()
	report, err := Run(Config{Root: root, Tolerance: 120 * time.Millisecond})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return report
}

func failureCounts(report *Report) map[string]int {
	counts := make(map[string]int)
	for _, failure := range report.Failures {
		counts[failure.Code]++
	}
	return counts
}

func requireCode(t *testing.T, report *Report, code string) {
	t.Helper()
	if failureCounts(report)[code] == 0 {
		t.Fatalf("missing %s in failures: %+v", code, report.Failures)
	}
}

func TestCleanReport(t *testing.T) {
	root := buildHLS(t, "clean", 3, 8, 0)
	report := verifyHLS(t, root)
	if report.Failed() || len(report.Failures) != 0 {
		t.Fatalf("clean report has failures: %+v", report.Failures)
	}
	want := []string{"1080p", "720p", "480p"}
	if len(report.Variants) != len(want) {
		t.Fatalf("variants = %d, want %d", len(report.Variants), len(want))
	}
	for i, variant := range report.Variants {
		if variant.Name != want[i] {
			t.Fatalf("variant[%d] = %s, want %s", i, variant.Name, want[i])
		}
		if variant.SegmentsFound != 8 || variant.SegmentsExpected != 8 {
			t.Fatalf("%s segments = %d/%d", variant.Name, variant.SegmentsFound, variant.SegmentsExpected)
		}
		if variant.BitrateAdvertisedKbps <= 0 || variant.BitrateMeasuredKbps <= 0 {
			t.Fatalf("%s bandwidth was not measured", variant.Name)
		}
	}
}

func TestGapReorderReport(t *testing.T) {
	root := buildHLS(t, "gap-reorder", 3, 30, 10)
	report := verifyHLS(t, root)
	if !report.Failed() || len(report.Failures) < 4 || len(report.Failures) > 8 {
		t.Fatalf("gap-reorder failures = %d, want 4..8: %+v", len(report.Failures), report.Failures)
	}
	for _, code := range []string{"SEG_GAP", "SEG_DUP", "SEG_ORDER", "EXTINF_DRIFT"} {
		requireCode(t, report, code)
	}
	if report.FirstErrorSequence == nil || *report.FirstErrorSequence != 21 {
		t.Fatalf("first error sequence = %v, want 21", report.FirstErrorSequence)
	}
	for _, variant := range report.Variants {
		if variant.SegmentsExpected != 10 || variant.SegmentsFound != 10 {
			t.Fatalf("%s expected/found = %d/%d", variant.Name, variant.SegmentsExpected, variant.SegmentsFound)
		}
		if variant.DurationDevMaxMS < 300 {
			t.Fatalf("%s duration dev max = %d", variant.Name, variant.DurationDevMaxMS)
		}
	}
	var first, second bytes.Buffer
	if err := report.WriteJSON(&first); err != nil {
		t.Fatal(err)
	}
	if err := report.WriteJSON(&second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("JSON output is not deterministic")
	}
}

func TestSegmentAnomalies(t *testing.T) {
	cases := map[string]string{
		"dup": "SEG_DUP", "truncated": "TS_TRUNCATED", "empty": "SEG_EMPTY", "pcr-rollback": "SEG_ORDER",
	}
	for scenario, code := range cases {
		t.Run(scenario, func(t *testing.T) {
			report := verifyHLS(t, buildHLS(t, scenario, 1, 8, 0))
			requireCode(t, report, code)
		})
	}
}

func TestMissingIntervalsAndDiskExtra(t *testing.T) {
	root := buildHLS(t, "clean", 1, 8, 0)
	for _, name := range []string{"000002.ts", "000003.ts"} {
		if err := os.Remove(filepath.Join(root, "1080p", name)); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(root, "1080p", "000004.ts")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "1080p", "999999-extra.ts"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	report := verifyHLS(t, root)
	counts := failureCounts(report)
	if counts["SEG_GAP"] != 1 {
		t.Fatalf("SEG_GAP count = %d, want merged interval count 1", counts["SEG_GAP"])
	}
	requireCode(t, report, "DISK_EXTRA")
	var gap Failure
	for _, failure := range report.Failures {
		if failure.Code == "SEG_GAP" {
			gap = failure
		}
	}
	if gap.Sequence != 2 || gap.EndSequence != 3 {
		t.Fatalf("merged gap = %d..%d, want 2..3", gap.Sequence, gap.EndSequence)
	}
}

func TestPCRRollbackAndJump(t *testing.T) {
	t.Run("rollback", func(t *testing.T) {
		root := buildHLS(t, "clean", 1, 8, 0)
		mediaPath := filepath.Join(root, "1080p", "index.m3u8")
		media, err := playlist.ParseMediaFile(mediaPath)
		if err != nil {
			t.Fatal(err)
		}
		media.Segments[2], media.Segments[3] = media.Segments[3], media.Segments[2]
		file, err := os.Create(mediaPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := media.Write(file); err != nil {
			t.Fatal(err)
		}
		file.Close()
		report := verifyHLS(t, root)
		requireCode(t, report, "PCR_ROLLBACK")
	})
	t.Run("jump", func(t *testing.T) {
		root := buildHLS(t, "clean", 1, 8, 0)
		path := filepath.Join(root, "1080p", "000004.ts")
		shiftFilePCR(t, path, 11*ts.PCRClock)
		report := verifyHLS(t, root)
		requireCode(t, report, "PCR_JUMP")
	})
}

func TestBandwidthMismatch(t *testing.T) {
	root := buildHLS(t, "clean", 1, 8, 0)
	masterPath := filepath.Join(root, "master.m3u8")
	data, err := os.ReadFile(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "BANDWIDTH=", "BANDWIDTH=9", 1)
	if err := os.WriteFile(masterPath, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	report := verifyHLS(t, root)
	requireCode(t, report, "BANDWIDTH_MISMATCH")
}

func TestWindowSequenceAndDiscontinuity(t *testing.T) {
	window, err := playlist.NewWindow(2*time.Second, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	frame := ts.DurationToTicks(time.Second / 30)
	for seq := int64(0); seq < 3; seq++ {
		start := seq * 2 * ts.PCRClock
		window.Add(playlist.WindowSegment{URI: "a.ts", FirstPCR: start, LastPCR: start + 2*ts.PCRClock - frame, Duration: 2 * time.Second})
	}
	if window.MediaSequence != 1 || window.Len() != 2 {
		t.Fatalf("media sequence/len = %d/%d, want 1/2", window.MediaSequence, window.Len())
	}
	if !window.Add(playlist.WindowSegment{URI: "gap.ts", FirstPCR: 10 * ts.PCRClock, LastPCR: 12*ts.PCRClock - frame, Duration: 2 * time.Second}) {
		t.Fatal("expected discontinuity insertion")
	}
	if window.DiscontinuitySequence != 1 {
		t.Fatalf("discontinuity sequence = %d, want 1", window.DiscontinuitySequence)
	}
	media := window.Playlist(true)
	if !media.Segments[len(media.Segments)-1].Discontinuity || !media.EndList {
		t.Fatalf("discontinuity/endlist missing: %+v", media)
	}
}

func TestLargeSegmentSetMemory(t *testing.T) {
	root := t.TempDir()
	if _, err := slicer.Generate(slicer.Config{
		OutputDir: filepath.Join(root, "live"), Segments: 1200,
		TargetDuration: 2 * time.Second, FrameRate: 30,
		Renditions: []slicer.Rendition{{Name: "1080p", Resolution: "1920x1080", Bitrate: 10000}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := gen.Generate(gen.Config{
		Input: filepath.Join(root, "live"), Output: filepath.Join(root, "hls"),
		TargetDuration: 2 * time.Second, EndList: true,
	}); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	report := verifyHLS(t, filepath.Join(root, "hls"))
	runtime.ReadMemStats(&after)
	if len(report.Failures) != 0 || report.Variants[0].SegmentsFound != 1200 {
		t.Fatalf("large verification failed: failures=%d found=%d", len(report.Failures), report.Variants[0].SegmentsFound)
	}
	if after.HeapAlloc > 256<<20 {
		t.Fatalf("heap after verification = %d bytes", after.HeapAlloc)
	}
	if after.TotalAlloc-before.TotalAlloc > 256<<20 {
		t.Fatalf("allocated during verification = %d bytes", after.TotalAlloc-before.TotalAlloc)
	}
}

func shiftFilePCR(t *testing.T, path string, delta int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset+ts.PacketSize <= len(data); offset += ts.PacketSize {
		packet := data[offset : offset+ts.PacketSize]
		if packet[0] != 0x47 || (packet[3]>>4)&0x03 < 2 || packet[4] < 7 || packet[5]&0x10 == 0 {
			continue
		}
		b := packet[6:12]
		base := uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 | uint64(b[3])<<1 | uint64(b[4])>>7
		ticks := int64(base)*300 + int64(uint16(b[4]&1)<<8|uint16(b[5])) + delta
		base = uint64(ticks / 300)
		ext := uint16(ticks % 300)
		b[0] = byte(base >> 25)
		b[1] = byte(base >> 17)
		b[2] = byte(base >> 9)
		b[3] = byte(base >> 1)
		b[4] = byte(base&1)<<7 | byte(ext>>8)
		b[5] = byte(ext)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
