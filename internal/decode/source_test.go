package decode

import (
	"context"
	"os"
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/fixture"
	"mediacore/internal/scan"
)

func buildSource(t *testing.T) (*Source, *scan.Scan) {
	t.Helper()
	m, err := fixture.Build(fixture.Options{Dir: t.TempDir(), Name: "dec", Segments: 4})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := scan.Run(m.Path, scan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Timeline == nil {
		t.Fatal("scan has no timeline")
	}
	src, err := Open(m.Path, sc.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	return src, sc
}

func TestDecodeFormatAndWindows(t *testing.T) {
	src, sc := buildSource(t)
	f := src.AudioFormat()
	if f.Rate != fixture.AudioRate || f.Channels != 2 {
		t.Fatalf("format: %+v", f)
	}
	// First second should hold about one second of 48k-equivalent source.
	b, err := src.ReadIntoBuffer(context.Background(), 0, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if b.Rate != fixture.AudioRate || b.Channels() != 2 {
		t.Fatalf("buffer %d Hz %d ch", b.Rate, b.Channels())
	}
	if got := b.Frames(); got < src.rate*8/10 || got > src.rate*12/10 {
		t.Fatalf("window produced %d frames, want about %d", got, src.rate)
	}
	// Second window must not overlap the first: total frames should approach
	// two seconds, not four.
	b2, err := src.ReadIntoBuffer(context.Background(), 1_000_000, 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	total := b.Frames() + b2.Frames()
	if total > src.rate*22/10 {
		t.Fatalf("windows overlap: %d frames for 2s", total)
	}
	if sc.DurationUS < 3_000_000 {
		t.Fatalf("fixture duration %d", sc.DurationUS)
	}
}

func TestDecodeCancellation(t *testing.T) {
	src, _ := buildSource(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.ReadIntoBuffer(ctx, 0, 1_000_000); err == nil {
		t.Fatal("cancelled context not observed")
	}
}

func TestDecodeMissingFile(t *testing.T) {
	tl, err := clock.NewTimeline([]clock.TrackSpec{{Track: 0, TimeBase: clock.Rational{Num: 1, Den: 1000}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open("/nonexistent/file.mp4", tl); err == nil {
		t.Fatal("missing file opened")
	}
	if _, err := Open(os.DevNull, nil); err == nil {
		t.Fatal("nil timeline accepted")
	}
}
