package media

import "testing"

func TestRationalNormalisation(t *testing.T) {
	r := NewRational(-4, -8)
	if r.Num != 1 || r.Den != 2 {
		t.Fatalf("NewRational(-4,-8) = %v, want 1/2", r)
	}
	if got := NewRational(0, 5); got.Num != 0 || got.Den != 1 {
		t.Fatalf("zero = %v", got)
	}
}

func TestRationalFloor(t *testing.T) {
	cases := []struct {
		r    Rational
		want int64
	}{
		{NewRational(3, 2), 1},
		{NewRational(-1, 2), -1},
		{NewRational(-3, 2), -2},
		{NewRational(5, 1), 5},
		{NewRational(0, 7), 0},
	}
	for _, tc := range cases {
		if got := tc.r.Floor(); got != tc.want {
			t.Errorf("%v.Floor() = %d, want %d", tc.r, got, tc.want)
		}
	}
}

func TestFrameIndexIsLeftClosedRightOpen(t *testing.T) {
	cases := []struct {
		at   Rational
		want int64
	}{
		{NewRational(0, 1), 0},
		{NewRational(1, 100), 0},     // 0.01 s -> frame 0
		{NewRational(1, 25), 1},      // exactly on the boundary -> frame 1
		{NewRational(39999, 1e6), 0}, // 0.039999 s -> frame 0
		{NewRational(40001, 1e6), 1}, // 0.040001 s -> frame 1
		{NewRational(2, 25), 2},
		{NewRational(24999, 25*1000), 24}, // 0.99996 s is still frame 24
	}
	for _, tc := range cases {
		if got := FrameIndex(tc.at); got != tc.want {
			t.Errorf("FrameIndex(%v) = %d, want %d", tc.at, got, tc.want)
		}
	}
}

func TestFrameTimeExact(t *testing.T) {
	// Adding TimeBase 250 times must land exactly on 10 s; floats drift.
	acc := NewRational(0, 1)
	for i := 0; i < 250; i++ {
		acc = acc.Add(TimeBase)
	}
	if acc.Num != 10 || acc.Den != 1 {
		t.Fatalf("250 * TimeBase = %v, want 10/1", acc)
	}
	if FrameIndex(acc) != 250 {
		t.Fatalf("FrameIndex(10s) = %d", FrameIndex(acc))
	}
}

func TestMediaErrorCarriesContext(t *testing.T) {
	err := Newf(KindCorrupt, "render.decode", "B", 7, "bad png")
	if !errIs(err, ErrCorrupt) {
		t.Fatalf("Is(ErrCorrupt) false")
	}
	if err.Track != "B" || !err.HasSeq || err.Seq != 7 {
		t.Fatalf("context lost: %+v", err)
	}
	if m := New(KindInput, "x", "", -1, nil); m.HasSeq {
		t.Fatalf("negative seq should clear HasSeq")
	}
}

func errIs(err error, target error) bool {
	type iser interface{ Is(error) bool }
	if i, ok := err.(iser); ok {
		return i.Is(target)
	}
	return false
}
