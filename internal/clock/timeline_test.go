package clock

import (
	"errors"
	"math"
	"testing"
)

func TestNewNormalizes(t *testing.T) {
	tm, err := New(3600, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Num != 1 || tm.Den != 25 {
		t.Fatalf("got %v, want 1/25", tm)
	}
	if _, err := New(1, 0); err == nil {
		t.Fatal("expected error for zero denominator")
	}
}

func TestEditListNonZeroStart(t *testing.T) {
	// Track at 48 kHz; movie timescale 1000.
	// Initial empty edit of 500 ms, then media from 0 for 10 s.
	tl, err := NewTimeline(48000, 1000, []Edit{
		{MediaTime: -1, Duration: 500, Empty: true},
		{MediaTime: 0, Duration: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	off, err := tl.PresentationOffset()
	if err != nil {
		t.Fatal(err)
	}
	if off.Millis() != 500 {
		t.Fatalf("offset = %d ms, want 500", off.Millis())
	}
	// Media sample at 1 s (48000 units) must land at 1.5 s presentation.
	p, err := tl.ToPresentation(48000)
	if err != nil {
		t.Fatal(err)
	}
	if p.Millis() != 1500 {
		t.Fatalf("presentation = %d ms, want 1500", p.Millis())
	}
}

func TestEditListMediaTimeOffset(t *testing.T) {
	// Non-zero start: presentation begins at media time 2 s (180000/90000).
	tl, err := NewTimeline(90000, 90000, []Edit{
		{MediaTime: 180000, Duration: 540000},
	})
	if err != nil {
		t.Fatal(err)
	}
	off, err := tl.PresentationOffset()
	if err != nil {
		t.Fatal(err)
	}
	if off.Millis() != -2000 {
		t.Fatalf("offset = %d ms, want -2000", off.Millis())
	}
	p, err := tl.ToPresentation(180000)
	if err != nil {
		t.Fatal(err)
	}
	if p.Cmp(Zero) != 0 {
		t.Fatalf("media 180000 should map to 0, got %v", p)
	}
}

func TestEditListOverlapRejected(t *testing.T) {
	// Edit 0 covers media [0, 90000); edit 1 starts at 45000 -> overlap.
	_, err := NewTimeline(90000, 90000, []Edit{
		{MediaTime: 0, Duration: 90000},
		{MediaTime: 45000, Duration: 90000},
	})
	if err == nil {
		t.Fatal("expected overlap error")
	}
}

func TestEditListOutOfRangeMediaTime(t *testing.T) {
	_, err := NewTimeline(90000, 90000, []Edit{
		{MediaTime: -5, Duration: 90000},
	})
	if err == nil {
		t.Fatal("expected negative media time error")
	}
}

func TestTimestampOverflow(t *testing.T) {
	if _, err := ToBase(math.MaxInt64-1, 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
	v, err := ToBase(7200*90000, 90000) // 2 h at 90 kHz
	if err != nil {
		t.Fatal(err)
	}
	if v != 7200*90000 {
		t.Fatalf("got %d", v)
	}
}

func TestAddSubCmpExact(t *testing.T) {
	a := Must(1, 3)
	b := Must(1, 6)
	s, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cmp(Must(1, 2)) != 0 {
		t.Fatalf("1/3+1/6 = %v, want 1/2", s)
	}
	d, err := a.Sub(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Cmp(Must(1, 6)) != 0 {
		t.Fatalf("1/3-1/6 = %v, want 1/6", d)
	}
}
