package mp4_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"archivearchivist/internal/model"
	"archivearchivist/internal/mp4"
	"archivearchivist/internal/testkit"
)

func writeVideo(t *testing.T, samples []testkit.SampleSpec) (string, []int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v.mp4")
	offsets, err := testkit.WriteMP4(path, testkit.TrackSpec{
		TrackID: 1, Kind: "video", Codec: "avc1", Timescale: 90000,
		Width: 320, Height: 180, Seed: 11, Samples: samples,
	})
	if err != nil {
		t.Fatal(err)
	}
	return path, offsets
}

func TestSampleTableReconstruction(t *testing.T) {
	samples := []testkit.SampleSpec{
		{Duration: 3000, Keyframe: true, Size: 100},
		{Duration: 3000, Size: 200},
		{Duration: 2000, Keyframe: true, Size: 50},
	}
	path, offsets := writeVideo(t, samples)
	sc, err := mp4.ScanFile(path, "V1")
	if err != nil {
		t.Fatal(err)
	}
	tr := sc.Tracks[0]
	if len(tr.Samples) != 3 {
		t.Fatalf("samples = %d", len(tr.Samples))
	}
	wantDTS := []int64{0, 3000, 6000}
	for i, s := range tr.Samples {
		if s.DTS != wantDTS[i] {
			t.Fatalf("sample %d dts = %d, want %d", i, s.DTS, wantDTS[i])
		}
		if s.Offset != offsets[i] {
			t.Fatalf("sample %d offset = %d, want %d", i, s.Offset, offsets[i])
		}
	}
	if !tr.Samples[0].Keyframe || tr.Samples[1].Keyframe || !tr.Samples[2].Keyframe {
		t.Fatalf("keyframe flags wrong: %+v", tr.Samples)
	}
	if got, want := tr.KeyframeGaps, []int64{6000}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("keyframe gaps = %v, want %v", got, want)
	}
	if tr.ConstantFrame {
		t.Fatalf("mixed durations reported as constant")
	}
}

func TestVFRUsesPerSampleDurations(t *testing.T) {
	samples := []testkit.SampleSpec{
		{Duration: 3000, Keyframe: true, Size: 60},
		{Duration: 7500, Size: 60},
		{Duration: 1500, Size: 60},
	}
	path, _ := writeVideo(t, samples)
	sc, err := mp4.ScanFile(path, "P1")
	if err != nil {
		t.Fatal(err)
	}
	tr := sc.Tracks[0]
	if tr.ConstantFrame {
		t.Fatalf("VFR reported constant")
	}
	if tr.Samples[2].DTS != 10500 {
		t.Fatalf("dts = %d, want 10500", tr.Samples[2].DTS)
	}
}

func TestMissingDurationIsolatedNotGuessed(t *testing.T) {
	// A zero delta cannot advance time; the sample must be isolated rather
	// than filled with an assumed frame rate.
	samples := []testkit.SampleSpec{
		{Duration: 3000, Keyframe: true, Size: 60},
		{Duration: 0, Size: 60},
		{Duration: 3000, Keyframe: true, Size: 60},
	}
	path, _ := writeVideo(t, samples)
	sc, err := mp4.ScanFile(path, "V1")
	if err != nil {
		t.Fatal(err)
	}
	tr := sc.Tracks[0]
	if !tr.Samples[1].Isolated {
		t.Fatalf("zero-duration sample not isolated: %+v", tr.Samples[1])
	}
	if tr.Samples[2].DTS != 3000 {
		t.Fatalf("isolated sample advanced the timeline: dts = %d", tr.Samples[2].DTS)
	}
}

func TestTruncationIsLocated(t *testing.T) {
	samples := make([]testkit.SampleSpec, 12)
	for i := range samples {
		samples[i] = testkit.SampleSpec{Duration: 3000, Keyframe: i%4 == 0, Size: 120}
	}
	path, _ := writeVideo(t, samples)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.Truncate(path, fi.Size()-250); err != nil {
		t.Fatal(err)
	}
	sc, err := mp4.ScanFile(path, "V1")
	if err != nil {
		t.Fatalf("scan must not fail on truncation: %v", err)
	}
	if !sc.Truncated {
		t.Fatalf("truncation not detected")
	}
	if !sc.Faults.Has(model.CodeTruncatedBox) {
		t.Fatalf("no truncated_box fault")
	}
	// The last sample must now fall outside the file.
	if len(sc.CorruptRanges) == 0 {
		t.Fatalf("truncated sample not marked corrupt")
	}
}

func TestCorruptSampleFlagged(t *testing.T) {
	samples := make([]testkit.SampleSpec, 6)
	for i := range samples {
		samples[i] = testkit.SampleSpec{Duration: 3000, Keyframe: i == 0, Size: 600}
	}
	samples[3].ZeroBytes = 600
	path, offsets := writeVideo(t, samples)
	sc, err := mp4.ScanFile(path, "V1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.CorruptRanges) != 1 {
		t.Fatalf("corrupt ranges = %v", sc.CorruptRanges)
	}
	if sc.CorruptRanges[0].Start != offsets[3] {
		t.Fatalf("corrupt start = %d, want %d", sc.CorruptRanges[0].Start, offsets[3])
	}
	if !sc.Tracks[0].Samples[3].Corrupt || sc.Tracks[0].Samples[2].Corrupt {
		t.Fatalf("wrong sample flagged")
	}
}

func TestGarbageInputDoesNotCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.mp4")
	if err := os.WriteFile(path, []byte("not an mp4 at all, just bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := mp4.ScanFile(path, "X")
	if err != nil {
		t.Fatalf("scan must not fail: %v", err)
	}
	if len(sc.Tracks) != 0 {
		t.Fatalf("junk parsed as %d tracks", len(sc.Tracks))
	}
	if len(sc.Faults) == 0 {
		t.Fatalf("junk produced no fault")
	}
}

func TestEmptyFileDoesNotCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.mp4")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := mp4.ScanFile(path, "X")
	if err != nil {
		t.Fatalf("scan must not fail: %v", err)
	}
	if len(sc.Faults) == 0 {
		t.Fatalf("empty file produced no fault")
	}
}

func TestHugeDeclaredSampleDoesNotAllocate(t *testing.T) {
	// A sample that declares 512 MiB must be probed in bounded memory and
	// classified as out of range rather than allocated wholesale.
	samples := []testkit.SampleSpec{
		{Duration: 3000, Keyframe: true, Size: 64},
		{Duration: 3000, Size: 700},
	}
	path, _ := writeVideo(t, samples)
	// Patch stsz so the second sample claims 512 MiB.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(b, []byte("stsz"))
	if idx < 0 {
		t.Fatal("stsz not found")
	}
	// Layout: type(4) version/flags(4) sample_size(4) sample_count(4) sizes...
	sizeOff := idx + 4 + 8 + 4
	binary.BigEndian.PutUint32(b[sizeOff:], 512<<20)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := mp4.ScanFile(path, "V1")
	if err != nil {
		t.Fatalf("scan must not fail: %v", err)
	}
	if len(sc.Tracks) != 1 || len(sc.Tracks[0].Samples) != 2 {
		t.Fatalf("unexpected scan: %+v", sc)
	}
	if !sc.Tracks[0].Samples[1].Corrupt {
		t.Fatalf("out-of-range sample not flagged corrupt")
	}
	if len(sc.CorruptRanges) == 0 {
		t.Fatalf("corrupt range not reported")
	}
}

// TestSilentAudioIsNotCorruption ensures a legitimate all-zero audio frame is
// not reported as damage, which would otherwise cascade into a false
// unrecoverable verdict.
func TestSilentAudioIsNotCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.mp4")
	samples := []testkit.SampleSpec{
		{Duration: 1024, Keyframe: true, Size: 600, ZeroBytes: 600},
		{Duration: 1024, Keyframe: true, Size: 600},
	}
	if _, err := testkit.WriteMP4(path, testkit.TrackSpec{
		TrackID: 2, Kind: "audio", Codec: "mp4a", Timescale: 48000,
		Channels: 2, SampleRate: 48000, Seed: 3, Samples: samples,
	}); err != nil {
		t.Fatal(err)
	}
	sc, err := mp4.ScanFile(path, "A1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.CorruptRanges) != 0 {
		t.Fatalf("silent audio flagged corrupt: %v", sc.CorruptRanges)
	}
}
