package clock

import "testing"

func TestRTPWraparound(t *testing.T) {
	c := NewRTPClock(90000, 0)
	// last value before wrap, then first values after wrap
	got := c.Unwrap(0xFFFFFFF0)
	if got != int64(0xFFFFFFF0) {
		t.Fatalf("first stamp = %x", got)
	}
	got = c.Unwrap(0x00000010)
	// delta across the 32-bit boundary must be +0x20
	want := int64(0xFFFFFFF0) + 0x20
	if got != want {
		t.Fatalf("wrapped stamp = %x, want %x", got, want)
	}
}

func TestRTPReorderNoWrap(t *testing.T) {
	c := NewRTPClock(90000, 0)
	first := c.Unwrap(1000)
	second := c.Unwrap(900) // late packet
	if first != 1000 || second != 900 {
		t.Fatalf("unexpected unwrap: %d %d", first, second)
	}
}

func TestToMicros(t *testing.T) {
	c := NewRTPClock(90000, 1_000_000) // anchor at +1s
	c.Unwrap(0)                        // pin RTP timestamp 0 to the anchor
	got := c.Convert(90000)            // exactly one RTP second after origin
	if got != 2_000_000 {
		t.Fatalf("micros = %d, want 2000000", got)
	}
}

func TestContainerClock(t *testing.T) {
	c, err := NewContainerClock(48000)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ToMicros(48000); got != MicrosPerSecond {
		t.Fatalf("micros = %d", got)
	}
	if _, err := NewContainerClock(0); err == nil {
		t.Fatal("zero timescale must be rejected")
	}
}

func TestTimeline(t *testing.T) {
	tl := NewTimeline(0)
	video := tl.AddRTP("video", 90000, 0)
	audio := tl.AddRTP("audio", 48000, 500_000) // audio starts 500ms later
	if got := video.Convert(0); got != 0 {
		t.Fatalf("video origin = %d", got)
	}
	if got := audio.Convert(0); got != 500_000 {
		t.Fatalf("audio origin = %d", got)
	}
	if d := OffsetMillis(audio.Convert(48000), video.Convert(90000)); d != 500 {
		t.Fatalf("offset ms = %d, want 500", d)
	}
}
