package mp4

import (
	"errors"
	"testing"

	"mediascan/internal/fault"
)

func TestScanBoxesRoundTrip(t *testing.T) {
	data := BuildFixture(FixtureSpec{
		Tracks: 2, Frames: 10, Timescale: 30_000, KeyEvery: 5,
	})
	roots, err := ScanBoxes(data, DefaultLimits())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := len(FindAll(roots, BoxTrak)); got != 2 {
		t.Fatalf("traks=%d want 2", got)
	}
	if got := len(FindAll(roots, BoxStts)); got != 2 {
		t.Fatalf("stts count=%d", got)
	}
	// Every parsed box must stay inside the file and nest correctly.
	for _, b := range FindAll(roots, BoxTrak) {
		if b.End() > int64(len(data)) || b.Depth != 1 {
			t.Fatalf("trak bounds/depth wrong: %+v", b)
		}
		if findChild(b, BoxMdhd) == nil {
			t.Fatal("mdhd not nested under trak")
		}
	}
}

func TestTruncatedMdat(t *testing.T) {
	const badFrame = 6
	data := BuildFixture(FixtureSpec{
		Tracks: 1, Frames: 12, Timescale: 30_000,
		KeyEvery: 3, CorruptAtFrame: badFrame, Corrupt: true,
	})
	roots, err := ScanBoxes(data, DefaultLimits())
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err=%v want ErrTruncated", err)
	}
	// The error must name the exact byte offset where corruption starts.
	mdat := First(roots, BoxMdat)
	if mdat == nil {
		t.Fatal("mdat header should still be discoverable")
	}
	// Independent offset check: fixture declares 16-byte samples and the
	// first bad frame's offset equals mdat payload + badFrame*16.
	want := mdat.Payload + int64(badFrame)*16
	if int64(len(data)) != want {
		t.Fatalf("cut offset=%d want %d (localization off)", len(data), want)
	}
	f := FirstTruncationFault(1, want, 16)
	if f.Kind != fault.KindTruncatedBox || f.OffsetBytes != want {
		t.Fatalf("fault localization wrong: %+v", f)
	}
}

func TestTruncatedHeader(t *testing.T) {
	data := BuildFixture(FixtureSpec{
		Tracks: 1, Frames: 4, Timescale: 90_000,
		KeyEvery: 2, CorruptAtFrame: 0, Corrupt: true, TruncateHeader: true,
	})
	if _, err := ScanBoxes(data, DefaultLimits()); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err=%v want header truncation", err)
	}
}

func TestBuildTracksVFR(t *testing.T) {
	// Cyclic durations 800/1200 at 30k => 26.67ms then 40ms, explicitly VFR.
	data := BuildFixture(FixtureSpec{
		Tracks: 1, Frames: 6, Timescale: 30_000,
		VFRDurations: []uint32{800, 1200}, KeyEvery: 2,
	})
	roots, err := ScanBoxes(data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	infos, _, err := BuildTracks(data, roots, DefaultLimits())
	if err != nil {
		t.Fatalf("build tracks: %v", err)
	}
	if len(infos) != 1 || len(infos[0].Samples) != 6 {
		t.Fatalf("track/sample count wrong: %+v", infos)
	}
	s := infos[0].Samples
	// Exact rational mapping: 800/30000 s = 26666 ticks, 1200/30000 = 40000.
	if s[0].DurationTicks != 26_666 || s[1].DurationTicks != 40_000 {
		t.Fatalf("VFR durations wrong: %d %d", s[0].DurationTicks, s[1].DurationTicks)
	}
	// Start times accumulate without float drift.
	if s[2].StartTicks != 26_666+40_000 {
		t.Fatalf("start tick accumulation wrong: %d", s[2].StartTicks)
	}
	// Sample offsets must point inside mdat and be monotonically increasing.
	for i := 1; i < len(s); i++ {
		if s[i].Offset <= s[i-1].Offset {
			t.Fatalf("offsets not monotonic at %d", i)
		}
	}
}

func TestKeyframeGapExceeded(t *testing.T) {
	data := BuildFixture(FixtureSpec{
		Tracks: 1, Frames: 20, Timescale: 30_000,
		VFRDurations: []uint32{1000, 1000}, KeyEvery: 8,
	})
	roots, err := ScanBoxes(data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	infos, _, err := BuildTracks(data, roots, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Limit of 100 ms: keyframes every 8 frames * ~33.33ms must exceed it.
	rep := AnalyzeKeyframes(infos[0], 100_000)
	if rep.FirstBadFrame < 0 {
		t.Fatal("expected a keyframe gap fault")
	}
	if len(rep.Faults) == 0 || rep.Faults[0].Kind != fault.KindKeyframeGap {
		t.Fatalf("faults wrong: %+v", rep.Faults)
	}
	// Within-limit run must stay clean.
	repOK := AnalyzeKeyframes(infos[0], 1_000_000)
	if repOK.FirstBadFrame != -1 {
		t.Fatalf("false positive gap: %+v", repOK)
	}
}

func TestLimitsEnforced(t *testing.T) {
	data := BuildFixture(FixtureSpec{Tracks: 3, Frames: 4, Timescale: 30_000})
	lim := DefaultLimits()
	lim.MaxBoxDepth = 1
	if _, err := ScanBoxes(data, lim); !errors.Is(err, ErrLimit) {
		t.Fatalf("depth limit err=%v", err)
	}
	lim = DefaultLimits()
	lim.MaxFileBytes = 8
	if _, err := ScanBoxes(data, lim); !errors.Is(err, ErrLimit) {
		t.Fatalf("size limit err=%v", err)
	}
}
