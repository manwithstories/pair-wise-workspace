package clock

import (
	"math/big"
	"testing"
)

func TestRescaleExactRational(t *testing.T) {
	video := MustNew(90000)
	audio := MustNew(48000)
	got, err := video.Rescale(90000, audio)
	if err != nil {
		t.Fatal(err)
	}
	if got != 48000 {
		t.Fatalf("rescale = %d, want 48000", got)
	}
	r, err := video.RescaleRat(3, audio)
	if err != nil {
		t.Fatal(err)
	}
	want := big.NewRat(8, 5)
	if r.Cmp(want) != 0 {
		t.Fatalf("exact ratio = %s, want %s", r.RatString(), want.RatString())
	}
	if _, err := video.Rescale(1<<40, MustNew(7)); err != nil {
		t.Fatalf("unexpected overflow: %v", err)
	}
	if _, err := MustNew(1).Rescale(^uint64(0), MustNew(90000)); err == nil {
		t.Fatal("expected overflow error for max ticks")
	}
}

func TestZeroTimescaleRejected(t *testing.T) {
	if _, err := New(0); err == nil {
		t.Fatal("expected zero timescale error")
	}
}

func TestMappingTruncatesWithoutFloat(t *testing.T) {
	m, err := NewMapping(MustNew(90000), MustNew(48000))
	if err != nil {
		t.Fatal(err)
	}
	ticks, exact, err := m.Map(30031)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 16016 {
		t.Fatalf("mapped = %d, want 16016", ticks)
	}
	if exact.Num().Int64() != 240248 || exact.Denom().Int64() != 15 {
		t.Fatalf("exact = %s", exact.RatString())
	}
}
