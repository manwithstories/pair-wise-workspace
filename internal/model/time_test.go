package model

import "testing"

func TestRationalExactArithmetic(t *testing.T) {
	// 1/3 + 1/6 = 1/2 exactly, no float representation.
	third := MustRational(1, 3)
	sixth := MustRational(1, 6)
	half := third.Add(sixth)
	if half.Num() != 1 || half.Den() != 2 {
		t.Fatalf("1/3+1/6 = %s, want 1/2", half)
	}
	// Cross-cancelling multiplication stays small and exact.
	got := MustRational(3, 4).Mul(MustRational(4, 3))
	if got.Num() != 1 || got.Den() != 1 {
		t.Fatalf("3/4*4/3 = %s, want 1", got)
	}
}

func TestRationalRejectsNonPositiveDenominator(t *testing.T) {
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("expected error for zero denominator")
	}
	if _, err := NewRational(1, -2); err == nil {
		t.Fatal("expected error for negative denominator")
	}
}

func TestMicrosecondsRoundsHalfAwayFromZero(t *testing.T) {
	// 1/3 s = 333333.333... us -> 333333
	if got := MustRational(1, 3).Microseconds(); got != 333333 {
		t.Fatalf("1/3 s = %d us, want 333333", got)
	}
	// 2/3 s = 666666.666... us -> 666667
	if got := MustRational(2, 3).Microseconds(); got != 666667 {
		t.Fatalf("2/3 s = %d us, want 666667", got)
	}
}

func TestConvertSampleIntegerMapping(t *testing.T) {
	got, err := ConvertSample(48000, 48000, 96000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 96000 {
		t.Fatalf("48k->96k = %d, want 96000", got)
	}
	if _, err := ConvertSample(1, 4000, 48000); err == nil {
		t.Fatal("expected error for out-of-range source rate")
	}
}

func TestSampleRateValidation(t *testing.T) {
	for _, sr := range []SampleRate{8000, 44100, 48000, 96000, 192000} {
		if !sr.Valid() {
			t.Fatalf("rate %d should be valid", sr)
		}
	}
	for _, sr := range []SampleRate{7999, 192001} {
		if sr.Valid() {
			t.Fatalf("rate %d should be invalid", sr)
		}
	}
}

func TestErrorLogKindAccounting(t *testing.T) {
	log := NewErrorLog()
	log.Addf(ErrBlockMissing, "t", 1, 10, "gone")
	log.Addf(ErrBlockCRC, "t", 2, 20, "bad crc")
	log.Addf(ErrBlockMissing, "t", 3, 30, "gone")
	if log.Count(ErrBlockMissing) != 2 {
		t.Fatal("missing count mismatch")
	}
	if !log.Has(ErrBlockCRC) || !log.Degraded() {
		t.Fatal("log should report crc defect and degradation")
	}
	log.Remove(0)
	if log.Count(ErrBlockMissing) != 1 {
		t.Fatal("remove did not drop the first entry")
	}
}
