package model

import (
	"errors"
	"testing"
)

func TestRat_ExactArithmetic(t *testing.T) {
	a := MustRat(1, 10)
	b := MustRat(2, 10)
	got, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	// 0.1 + 0.2 must be exactly 3/10, not a float artifact.
	if got.Num() != 3 || got.Den() != 10 {
		t.Fatalf("0.1+0.2 = %s, want 3/10", got)
	}
	ms, err := got.Millis()
	if err != nil || ms != 300 {
		t.Fatalf("Millis = %d, %v", ms, err)
	}
}

func TestRat_MulSubCmpAndNormalization(t *testing.T) {
	r, _ := NewRat(4, 6) // normalizes to 2/3
	if r.Num() != 2 || r.Den() != 3 {
		t.Fatalf("normalization = %s", r)
	}
	p, err := r.Mul(MustRat(3, 2))
	if err != nil || p.Num() != 1 || p.Den() != 1 {
		t.Fatalf("2/3 * 3/2 = %s, %v", p, err)
	}
	diff, _ := MustRat(5, 4).Sub(MustRat(1, 4))
	if diff.Num() != 1 || diff.Den() != 1 {
		t.Fatalf("5/4-1/4 = %s", diff)
	}
	if MustRat(1, 3).Cmp(MustRat(2, 3)) != -1 || MustRat(1, 1).Cmp(MustRat(1, 1)) != 0 {
		t.Fatal("Cmp ordering wrong")
	}
}

func TestRat_RejectsBadDenominatorAndOverflow(t *testing.T) {
	if _, err := NewRat(1, 0); !errors.Is(err, ErrOverflow) {
		t.Fatalf("zero denominator must fail, got %v", err)
	}
	if _, err := NewRat(1, -2); !errors.Is(err, ErrOverflow) {
		t.Fatalf("negative denominator must fail, got %v", err)
	}
	huge := MustRat(1<<62, 1)
	if _, err := huge.Mul(huge); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow must be detected, got %v", err)
	}
	if _, err := Seconds(1, 0); !errors.Is(err, ErrOverflow) {
		t.Fatal("Seconds with zero timescale must fail")
	}
}

func TestTimeline_AddRejectsBadDurations(t *testing.T) {
	tl := NewTimeline()
	good := SegmentRef{Track: TrackStudio, Index: 0, Duration: MustRat(1, 1)}
	if err := tl.Add(good); err != nil {
		t.Fatal(err)
	}
	bad := SegmentRef{Track: TrackStudio, Index: 1, Duration: MustRat(0, 1)}
	if err := tl.Add(bad); !errors.Is(err, ErrNegativeDuration) {
		t.Fatalf("zero duration must fail, got %v", err)
	}
	negByte := SegmentRef{Track: TrackField, Index: 0, Duration: MustRat(1, 1), ByteStart: -1}
	if err := tl.Add(negByte); err == nil {
		t.Fatal("negative byte start must fail")
	}
}

func TestTimeline_MarkPartialAndMediaError(t *testing.T) {
	tl := NewTimeline()
	tl.MarkPartial(MissingDependency{Track: TrackVoice, Index: 7, URI: "v.m4s"})
	if !tl.Partial || len(tl.Missing) != 1 || tl.Missing[0].Index != 7 {
		t.Fatalf("partial marker wrong: %+v", tl.Missing)
	}
	me := NewMediaError("walk", TrackField, 3, 4096, true, ErrTruncated)
	if me.Offset != 4096 || !me.Recoverable || me.Track != TrackField {
		t.Fatalf("media error provenance lost: %+v", me)
	}
	if !errors.Is(me, ErrTruncated) {
		t.Fatal("Unwrap must expose the sentinel cause")
	}
}
