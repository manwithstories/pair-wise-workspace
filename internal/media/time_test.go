package media

import "testing"

func TestTimeReduces(t *testing.T) {
	if tm := MustTime(2, 4); tm.Num != 1 || tm.Den != 2 {
		t.Fatalf("want 1/2, got %s", tm)
	}
	if tm, err := NewTime(0, 7); err != nil || tm.Num != 0 || tm.Den != 1 {
		t.Fatalf("zero should reduce to 0/1: %v %v", tm, err)
	}
}

func TestTimeRejectsBadInput(t *testing.T) {
	if _, err := NewTime(1, 0); err == nil || err.Class != ClassInput {
		t.Fatalf("zero denominator must be class-2, got %v", err)
	}
	if _, err := NewTime(-1, 2); err == nil || err.Code != "time.negative" {
		t.Fatalf("negative time must be rejected, got %v", err)
	}
}

func TestSinceRejectsReversed(t *testing.T) {
	a, b := MustTime(1, 3), MustTime(1, 2)
	if _, err := b.Since(a); err != nil {
		t.Fatalf("valid subtraction failed: %v", err)
	}
	if _, err := a.Since(b); err == nil || err.Class != ClassInput {
		t.Fatalf("reversed subtraction must be class-2, got %v", err)
	}
}

func TestUnitsRounding(t *testing.T) {
	cases := []struct {
		tm   MediaTime
		unit int64
		want int64
	}{
		{MustTime(1, 3), AudioRate, 16000},
		{MustTime(1, 7), AudioRate, 6857},
		{MustTime(2, 7), AudioRate, 13714},
		{MustTime(1, 24), VideoFPS, 1},
		{MustTime(5, 1000), AudioRate, 240},
		{MustTime(3, 2), AudioRate, 72000},
	}
	for _, c := range cases {
		got, err := c.tm.Units(c.unit)
		if err != nil {
			t.Fatalf("%s: %v", c.tm, err)
		}
		if got != c.want {
			t.Fatalf("%s -> %d units, want %d", c.tm, got, c.want)
		}
	}
}

func TestUnitsRoundsHalfUp(t *testing.T) {
	// 1/96000 s is exactly half a 48 kHz sample; the tie rounds up to 1.
	if got, err := MustTime(1, 96000).SampleIndex(); err != nil || got != 1 {
		t.Fatalf("tie should round to 1 sample, got %d %v", got, err)
	}
	if _, err := MustTime(0, 1).Units(0); err == nil {
		t.Fatal("zero units must be rejected")
	}
}

func TestCompareExactAcrossDenominators(t *testing.T) {
	if MustTime(44100, 44100).Cmp(MustTime(48000, 48000)) != 0 {
		t.Fatal("1 != 1 across denominators")
	}
	if MustTime(1, 2).Cmp(MustTime(2, 3)) != -1 {
		t.Fatal("1/2 should precede 2/3")
	}
	if MustTime(1, 44100).Cmp(MustTime(1, 48000)) != 1 {
		t.Fatal("1/44100 is a longer interval than 1/48000")
	}
}

func TestOverflowIsInputError(t *testing.T) {
	big := MediaTime{Num: maxInt64, Den: 1}
	if _, err := big.Units(AudioRate); err == nil || err.Class != ClassInput {
		t.Fatalf("overflow must be class-2, got %v", err)
	}
	if _, err := big.Add(MustTime(1, 1)); err == nil || err.Class != ClassInput {
		t.Fatalf("add overflow must be class-2, got %v", err)
	}
	if _, err := big.Since(MustTime(1, 3)); err == nil || err.Class != ClassInput {
		t.Fatalf("since overflow must be class-2, got %v", err)
	}
}
