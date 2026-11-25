package clock

import (
	"errors"
	"testing"
)

func TestTickRescaleNoDrift(t *testing.T) {
	// 44100 -> 48000 on a known sample boundary: 44100 ticks (1s) -> 48000.
	a, err := New(44100, 44100)
	if err != nil {
		t.Fatal(err)
	}
	b, err := a.To(48000)
	if err != nil {
		t.Fatal(err)
	}
	if b.Value != 48000 || b.Timescale != 48000 {
		t.Fatalf("rescale = %+v, want 48000/48000", b)
	}
	// Repeated rescale over 1000 one-ms steps must not drift (48k base).
	var acc Tick
	acc.Timescale = 48000
	oneMs, err := New(48, 48000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		acc.Value += oneMs.Value
	}
	if acc.Value != 48000 {
		t.Fatalf("accumulated drift: %d, want 48000", acc.Value)
	}
}

func TestTickCmp(t *testing.T) {
	cases := []struct {
		v1, ts1 uint64
		v2      uint64
		ts2     uint32
		want    int
	}{
		{1000, 1000, 1, 1, 0},
		{1, 2, 1, 3, 1},      // 0.5s vs 0.333s
		{1, 3, 1, 2, -1},
		{44100, 44100, 48000, 48000, 0},
	}
	for _, c := range cases {
		a, _ := New(c.v1, uint32(c.ts1))
		b, _ := New(c.v2, c.ts2)
		got, err := a.Cmp(b)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Fatalf("Cmp(%v,%v)=%d want %d", a, b, got, c.want)
		}
	}
}

func TestMillisRounding(t *testing.T) {
	tk, _ := New(24, 48000) // 0.5ms -> rounds up to 1
	if tk.Millis() != 1 {
		t.Fatalf("half-up millis = %d, want 1", tk.Millis())
	}
	d, _ := NewDuration(23, 48000) // < 0.5ms -> 0
	if d.Millis() != 0 {
		t.Fatalf("duration millis = %d, want 0", d.Millis())
	}
}

func TestAddSub(t *testing.T) {
	a, _ := New(100, 90000)
	d, _ := NewDuration(50, 90000)
	b, err := a.Add(d)
	if err != nil || b.Value != 150 {
		t.Fatalf("add = %v %v", b, err)
	}
	got, err := Sub(b, a)
	if err != nil || got.Ticks != 50 {
		t.Fatalf("sub = %v %v", got, err)
	}
	if _, err := a.Add(Duration{Ticks: 1, Timescale: 1000}); err == nil {
		t.Fatal("timescale mismatch must error")
	}
}

func TestZeroTimescaleRejected(t *testing.T) {
	if _, err := New(1, 0); err == nil {
		t.Fatal("zero timescale accepted")
	}
	if _, err := New(1, 1000); err != nil {
		t.Fatal(err)
	}
}

func TestErrorModel(t *testing.T) {
	tk, _ := New(77, 90000)
	err := E(KindCorrupt, "scan", WithTrack(2), WithTick(tk), WithOffset(1234), WithDetail("bad box"))
	if ExitCode(err) != 3 {
		t.Fatalf("exit = %d, want 3", ExitCode(err))
	}
	if !IsKind(err, KindCorrupt) {
		t.Fatal("IsKind failed")
	}
	if s := err.Error(); s == "" {
		t.Fatal("empty error string")
	}
	wrapped := E(KindInconsistent, "resume", WithWrap(errors.New("checksum mismatch")))
	if ExitCode(wrapped) != 4 {
		t.Fatalf("exit = %d, want 4", ExitCode(wrapped))
	}
	var pe *Error
	if !errors.As(wrapped, &pe) {
		t.Fatal("errors.As failed")
	}
	for k, want := range map[ErrorKind]int{
		KindArgument: 2, KindCorrupt: 3, KindInconsistent: 4, KindCanceled: 130,
	} {
		if k.ExitCode() != want {
			t.Fatalf("kind %d -> %d, want %d", k, k.ExitCode(), want)
		}
	}
}
