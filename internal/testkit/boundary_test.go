package testkit

import (
	"os"
	"path/filepath"
	"testing"

	"medialine/internal/clock"
	"medialine/internal/fault"
	"medialine/internal/mp4"
)

func TestGenerateRTPStreamWraps(t *testing.T) {
	dir := t.TempDir()
	stream, err := GenerateRTPStream(dir, RTPStreamSpec{
		Name: "wrap", ClockRate: 90_000, Count: 8,
		TimestampStep: 3_000, StartTimestamp: 4_294_967_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stream.Wraps != 1 {
		t.Fatalf("wraps=%d", stream.Wraps)
	}
	packets, err := ReadRTPStream(stream.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != stream.Spec.Count {
		t.Fatalf("packets=%d", len(packets))
	}
	timeline, err := clock.NewRTPTimeline(90_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range packets {
		got, err := timeline.Push(&packets[i])
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if got != stream.TimesUS[i] {
			t.Fatalf("packet %d time %d want %d", i, got, stream.TimesUS[i])
		}
	}
}

func TestGenerateRTPStreamLoss(t *testing.T) {
	stream, err := GenerateRTPStream(t.TempDir(), RTPStreamSpec{
		Name: "loss", ClockRate: 48_000, Count: 10, TimestampStep: 960,
		SequenceGapAfter: 3, StartSeq: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	packets, err := ReadRTPStream(stream.Path)
	if err != nil {
		t.Fatal(err)
	}
	timeline, _ := clock.NewRTPTimeline(48_000, 0)
	for i := range packets {
		if _, err := timeline.Push(&packets[i]); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if timeline.Lost() != 2 {
		t.Fatalf("lost=%d", timeline.Lost())
	}
}

func TestGenerateRTPStreamRejectsBadSpec(t *testing.T) {
	if _, err := GenerateRTPStream(t.TempDir(), RTPStreamSpec{Name: "x"}); err == nil {
		t.Fatal("expected spec error")
	}
}

func TestReadRTPStreamRejectsTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.rtp")
	if err := writeFile(path, []byte{0, 0, 0, 40, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRTPStream(path); err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestHugeLengthClipDetected(t *testing.T) {
	clip, err := HugeLengthClip(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.Parse(clip.Path, mp4.Options{})
	if err != nil {
		fe, _ := fault.As(err)
		if fe.Kind != fault.KindInvalidLength {
			t.Fatalf("expected invalid length, got %v", err)
		}
		return
	}
	if len(f.Issues) == 0 {
		t.Fatalf("expected an invalid-length issue")
	}
}

func TestZeroDurationClipDetected(t *testing.T) {
	clip, err := ZeroDurationClip(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.Parse(clip.Path, mp4.Options{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var zeroDur bool
	for _, tr := range f.Tracks {
		for _, s := range tr.Samples {
			if s.DurationUS == 0 {
				zeroDur = true
			}
		}
	}
	if !zeroDur {
		t.Fatal("expected at least one zero-duration sample")
	}
	// Multi-second zero durations must not be silently accepted by the writer
	// path; the track still parses so the caller can decide.
	if len(f.Tracks) == 0 {
		t.Fatal("track should still be present")
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
