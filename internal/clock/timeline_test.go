package clock

import (
	"testing"

	"archive/internal/model"
)

// TestUnwrapMonotonic verifies ordinary increments stay monotonic.
func TestUnwrapMonotonic(t *testing.T) {
	var u Unwrapper
	prev := int64(-1)
	for _, v := range []uint32{0, 3000, 6000, 9000, 12000} {
		ext, err := u.Unwrap(v)
		if err != nil {
			t.Fatal(err)
		}
		if ext <= prev {
			t.Fatalf("not monotonic: %d after %d", ext, prev)
		}
		prev = ext
	}
}

// TestUnwrapWrapAround verifies a 32-bit rollover extends correctly.
func TestUnwrapWrapAround(t *testing.T) {
	var u Unwrapper
	first, err := u.Unwrap(0xfffffff0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := u.Unwrap(0x00000010)
	if err != nil {
		t.Fatalf("wrap should be corrected, got error: %v", err)
	}
	if second <= first {
		t.Fatalf("wrap extended backwards: %d -> %d", first, second)
	}
	if second-first != 0x20 {
		t.Errorf("wrap delta = %d, want 32", second-first)
	}
}

// TestMapperSelfAnchor verifies a stream without an RTCP report maps from a
// zero origin and reports the missing report.
func TestMapperSelfAnchor(t *testing.T) {
	m := NewMapper(7, 90000)
	n0, err := m.ToNanos(0)
	if err != nil {
		t.Fatal(err)
	}
	n1, err := m.ToNanos(90000)
	if err != nil {
		t.Fatal(err)
	}
	if n0 != 0 {
		t.Errorf("first self-anchored time = %d, want 0", n0)
	}
	if n1 != 1_000_000_000 {
		t.Errorf("one second = %d, want 1e9", n1)
	}
	if !m.MissingReport() {
		t.Error("expected MissingReport true before any SR")
	}
}

// TestMapperAnchorImmutable verifies a later SR cannot move an anchor that
// already exists (regression for retroactive re-anchoring).
func TestMapperAnchorImmutable(t *testing.T) {
	m := NewMapper(7, 90000)
	if _, err := m.ToNanos(0); err != nil {
		t.Fatal(err)
	}
	m.ApplySR(model.SenderReport{SSRC: 7, RTPTime: 100, NTPTime: 3898444800 << 32, Valid: true})
	n, err := m.ToNanos(90000)
	if err != nil {
		t.Fatal(err)
	}
	// Self-anchored origin must survive: 90000 ticks at 90kHz is 1 second.
	if n != 1_000_000_000 {
		t.Errorf("anchor moved by late SR: ns=%d", n)
	}
}

// TestMapperRejectsCorruptSR verifies an invalid report never anchors.
func TestMapperRejectsCorruptSR(t *testing.T) {
	m := NewMapper(7, 90000)
	m.ApplySR(model.SenderReport{SSRC: 7, Valid: false})
	if m.ReportsReceived() != 1 {
		t.Error("corrupt report should still be counted")
	}
	if !m.MissingReport() {
		t.Error("corrupt report must not become an anchor")
	}
}

// TestMapperTimestampWrap verifies a packet stream crossing the 32-bit RTP
// boundary keeps advancing in nanoseconds rather than jumping backwards.
// Packets use a realistic small interval; the first value is placed near the
// 32-bit edge so a handful of packets cross it.
func TestMapperTimestampWrap(t *testing.T) {
	m := NewMapper(9, 90000)
	const step uint32 = 3000 // normal video interval (~33 ms)
	start := uint32(0xfffff000)
	var prev int64
	for i := uint32(0); i < 4000; i++ {
		ns, err := m.ToNanos(start + i*step)
		if err != nil {
			t.Fatalf("wrap produced a clock error at packet %d: %v", i, err)
		}
		if i > 0 && ns <= prev {
			t.Fatalf("non-monotonic across wrap at packet %d: %d -> %d", i, prev, ns)
		}
		prev = ns
	}
}

// TestRescale verifies exact timebase conversion and zero rejection.
func TestRescale(t *testing.T) {
	if got, _ := Rescale(90000, 90000, 1000); got != 1000 {
		t.Errorf("rescale = %d, want 1000", got)
	}
	if _, err := Rescale(1, 0, 1000); err == nil {
		t.Error("expected error on zero source timescale")
	}
}
