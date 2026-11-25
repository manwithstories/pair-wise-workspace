package media_test

import (
	"bytes"
	"errors"
	"math"
	"testing"

	mp4 "github.com/abema/go-mp4"

	"avcheck/internal/media"
)

// TestClockMatchesMP4Timescale cross-checks the integer rational clock against
// go-mp4's independent 32-bit timescale conversion. go-mp4 reports durations in
// units of 1/timescale seconds; avcheck must land on the same nanosecond value
// for every sample, with the same half-away-from-zero rounding.
func TestClockMatchesMP4Timescale(t *testing.T) {
	timescales := []uint32{1000, 48000, 90000, 44100, 30000, 1000000}
	samples := []int64{0, 1, 7, 1000, 48001, 123456789, 1 << 40}
	for _, ts := range timescales {
		// go-mp4 encodes an absolute media time and decodes it back; that
		// round-trip is the independent oracle for "N ticks at ts Hz".
		for _, n := range samples {
			box := &mp4.Mdhd{
				FullBox:    mp4.FullBox{Version: 0},
				Timescale:  ts,
				DurationV0: uint32(n & 0xffffffff),
			}
			var buf bytes.Buffer
			if _, err := mp4.Marshal(&buf, box, mp4.Context{}); err != nil {
				t.Fatalf("marshal mdhd ts=%d: %v", ts, err)
			}
			raw := buf.Bytes()
			got, _, err := mp4.UnmarshalAny(bytes.NewReader(raw), mp4.BoxTypeMdhd(),
				uint64(len(raw)), mp4.Context{})
			if err != nil {
				t.Fatalf("unmarshal mdhd ts=%d: %v", ts, err)
			}
			mdhd, ok := got.(*mp4.Mdhd)
			if !ok {
				t.Fatalf("unmarshal mdhd ts=%d: type %T", ts, got)
			}
			if mdhd.Timescale != ts {
				t.Fatalf("timescale round-trip = %d, want %d", mdhd.Timescale, ts)
			}
			// Oracle: seconds = ticks / ts, ns = round(ticks * 1e9 / ts)
			want := mulDivHalfAway(int64(mdhd.DurationV0), 1e9, int64(ts))
			gotNs := media.Nanos(int64(mdhd.DurationV0), media.Rational{Num: 1e9, Den: int64(ts)})
			if gotNs != want {
				t.Fatalf("ts=%d ticks=%d: got %d ns, want %d ns", ts, mdhd.DurationV0, gotNs, want)
			}
		}
	}
}

func mulDivHalfAway(a, b, d int64) int64 {
	x := a * b
	q := x / d
	rem := x % d
	if rem < 0 {
		rem = -rem
	}
	if 2*rem >= d {
		if x >= 0 {
			q++
		} else {
			q--
		}
	}
	return q
}

func TestNanosRounding(t *testing.T) {
	r := media.Rational{Num: 1, Den: 3}
	// 1 tick = 0.333... ns → 0; 2 ticks = 0.666... → 1; 2.5 ticks rounds
	// half away from zero; negatives mirror positives.
	cases := []struct {
		ticks, want int64
	}{
		{0, 0}, {1, 0}, {2, 1}, {5, 2}, {15, 5}, {-2, -1}, {-5, -2},
	}
	for _, c := range cases {
		if got := media.Nanos(c.ticks, r); got != c.want {
			t.Errorf("Nanos(%d, 1/3) = %d, want %d", c.ticks, got, c.want)
		}
	}
}

func TestNanosSaturates(t *testing.T) {
	r := media.Rational{Num: math.MaxInt64, Den: 1}
	if got := media.Nanos(math.MaxInt64, r); got != math.MaxInt64 {
		t.Fatalf("overflowing Nanos = %d, want saturation at MaxInt64", got)
	}
}

func TestDurationTicks(t *testing.T) {
	r := media.Rational{Num: 1_000_000, Den: 1} // 1 ms per tick
	if got := media.DurationToTicks(2_500_000, r); got != 3 {
		t.Fatalf("2.5ms at 1ms/tick = %d, want 3 (half away from zero)", got)
	}
	// A Duration of exactly one tick must round-trip.
	if got := media.DurationToTicks(1_000_000, r); got != 1 {
		t.Fatalf("1ms = %d ticks, want 1", got)
	}
}

func TestFaultExitCodes(t *testing.T) {
	cases := []struct {
		level media.Level
		want  int
	}{
		{media.LevelRecovered, media.ExitOK},
		{media.LevelDegraded, media.ExitOK},
		{media.LevelFatal, media.ExitOK},
		{media.LevelAbort, media.ExitCorrupt},
	}
	for _, c := range cases {
		fs := &media.Faults{}
		fs.Addf(c.level, media.CodeTruncatedVint, 0, 0, "x")
		if got := media.ExitCode(fs); got != c.want {
			t.Errorf("ExitCode(%s) = %d, want %d", c.level, got, c.want)
		}
	}
	if got := media.ExitCode(nil); got != media.ExitOK {
		t.Errorf("ExitCode(nil) = %d, want 0", got)
	}
	if got := media.ExitCode(errors.New("boom")); got != media.ExitInternal {
		t.Errorf("ExitCode(generic) = %d, want 1", got)
	}
}

func TestFaultsIsCode(t *testing.T) {
	fs := &media.Faults{}
	fs.Addf(media.LevelAbort, media.CodeTruncatedVint, 3, 17, "cut")
	if !errors.Is(fs, media.CodeTruncatedVint.Err()) {
		t.Error("errors.Is did not match the fault code")
	}
	if errors.Is(fs, media.CodePacketGap.Err()) {
		t.Error("errors.Is matched an absent code")
	}
	if !errors.Is(fs, media.LevelAbort.Err()) {
		t.Error("errors.Is did not match the fault level")
	}
	if fs.MaxLevel() != media.LevelAbort {
		t.Errorf("MaxLevel = %v, want abort", fs.MaxLevel())
	}
}

func TestFaultMaxLevelIsWorst(t *testing.T) {
	fs := &media.Faults{}
	fs.Addf(media.LevelRecovered, media.CodePacketRecovered, 1, 0, "r")
	fs.Addf(media.LevelFatal, media.CodeElementTooLarge, 1, 9, "f")
	fs.Addf(media.LevelDegraded, media.CodePacketGap, 1, 5, "d")
	if fs.MaxLevel() != media.LevelFatal {
		t.Fatalf("MaxLevel = %v, want fatal", fs.MaxLevel())
	}
	if _, ok := fs.First(media.CodePacketGap); !ok {
		t.Fatal("First did not find packet_gap")
	}
}
