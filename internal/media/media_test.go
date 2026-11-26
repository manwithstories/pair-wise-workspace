package media

import (
	"errors"
	"testing"
	"time"
)

func TestClockTicksAndDuration(t *testing.T) {
	if got := DurationToTicks(20 * time.Millisecond); got != 1800 {
		t.Fatalf("20ms ticks = %d, want 1800", got)
	}
	if got := TicksToMS(1920); got < 21.33 || got > 21.34 {
		t.Fatalf("AAC 1024/48k duration = %.3fms", got)
	}
	if got := TicksToDuration(90_000); got != time.Second {
		t.Fatalf("90k ticks = %v, want 1s", got)
	}
}

func TestTimestampExtenderWrap(t *testing.T) {
	var x TimestampExtender
	start := uint32(0xffff_ff00)
	want := []uint64{
		uint64(start), uint64(start + 1), uint64(start + 2),
		1<<32 + 3, 1<<32 + 4,
	}
	in := []uint32{start, start + 1, start + 2, 3, 4}
	for i, ts := range in {
		if got := x.Extend(ts); got != want[i] {
			t.Fatalf("Extend(%d) = %d, want %d", ts, got, want[i])
		}
	}
}

func TestTimestampExtenderAcceptsNearWrapReordering(t *testing.T) {
	var x TimestampExtender
	if got := x.Extend(0xffff_fff0); got != 0xffff_fff0 {
		t.Fatal(got)
	}
	if got := x.Extend(2); got != 1<<32+2 {
		t.Fatalf("post-wrap ts = %d", got)
	}
	// A late pre-wrap timestamp remains close to the established window.
	if got := x.Extend(0xffff_ffff); got != 0xffff_ffff {
		t.Fatalf("late pre-wrap ts = %d", got)
	}
}

func TestParseOpusTOCDurations(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		frames  int
		ms      float64
	}{
		{"silk 20ms code0", []byte{0x08}, 1, 20},   // config 1
		{"celt 2.5ms code0", []byte{0x80}, 1, 2.5}, // config 16
		{"two equal frames", []byte{0x09}, 2, 40},
		{"code3 three frames", []byte{0x0b, 0x03}, 3, 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseOpusTOC(tt.payload)
			if err != nil {
				t.Fatal(err)
			}
			if d.FrameCount != tt.frames {
				t.Fatalf("frames=%d want %d", d.FrameCount, tt.frames)
			}
			if gotMS := d.Duration.Seconds() * 1000; gotMS != tt.ms {
				t.Fatalf("duration=%v (%.2fms) want %.1fms", d.Duration, gotMS, tt.ms)
			}
		})
	}
}

func TestParseOpusTOCTruncated(t *testing.T) {
	_, err := ParseOpusTOC([]byte{0x0b})
	if Classify(err) != KindMalformed {
		t.Fatalf("err=%v kind=%s", err, Classify(err))
	}
}

func TestAACDurationAndAUHeaders(t *testing.T) {
	d, err := AACFrameDuration(48_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.ClockTicks != 1920 {
		t.Fatalf("AAC ticks=%d want 1920", d.ClockTicks)
	}
	payload := []byte{
		0x00, 0x10, // 16 header bits
		0x07, 0xf8, // AU size 255 << 3
		0xaa, 0xbb, 0xcc,
	}
	sizes, desc, err := ParseAACAUHeaders(payload, 48_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 1 || sizes[0] != 255 || desc.FrameCount != 1 {
		t.Fatalf("sizes=%v desc=%+v", sizes, desc)
	}
	if _, _, err := ParseAACAUHeaders([]byte{0, 16, 0}, 48_000); Classify(err) != KindMalformed {
		t.Fatalf("truncated headers err=%v", err)
	}
}

func TestErrorClassificationAndExitCode(t *testing.T) {
	malformed := New(KindMalformed, "test", "x")
	if Classify(malformed) != KindMalformed || ExitCode(malformed) != 2 {
		t.Fatal("malformed must exit 2")
	}
	wrapped := Wrap(KindConflict, "ingest", "id", errors.New("payload mismatch"))
	if !errors.Is(wrapped, wrapped.(*Error).Err) {
		t.Fatal("wrapped cause unavailable")
	}
	if Classify(wrapped) != KindConflict || ExitCode(wrapped) != 2 {
		t.Fatal("conflict must be classified and exit 2")
	}
	internal := errors.New("boom")
	if Classify(internal) != KindInternal || ExitCode(internal) != 1 {
		t.Fatal("unknown errors must be internal/exit 1")
	}
	if ExitCode(nil) != 0 {
		t.Fatal("success must exit 0")
	}
}
