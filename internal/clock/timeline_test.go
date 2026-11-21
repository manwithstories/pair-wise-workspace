package clock

import "testing"

func TestNormalize(t *testing.T) {
	r := New(4, 8)
	if r.Num != 1 || r.Den != 2 {
		t.Fatalf("normalize: got %v", r)
	}
	if New(1, 0).Den != 1 {
		t.Fatal("zero denominator must become 1")
	}
	if r := New(3, -6); r.Num != -1 || r.Den != 2 {
		t.Fatalf("negative den: got %v", r)
	}
}

func TestArithmetic(t *testing.T) {
	a, b := New(1, 3), New(1, 6)
	if got := a.Add(b); got != New(1, 2) {
		t.Fatalf("add: got %v", got)
	}
	if got := a.Sub(b); got != New(1, 6) {
		t.Fatalf("sub: got %v", got)
	}
	if a.Cmp(b) <= 0 || b.Cmp(a) >= 0 || a.Cmp(a) != 0 {
		t.Fatal("cmp")
	}
}

func TestSampleConversion(t *testing.T) {
	tm := SamplesToTime(48000, 48000)
	if tm.Seconds() != 1 {
		t.Fatalf("1s: got %v", tm.Seconds())
	}
	if got := New(1, 2).Samples(48000); got != 24000 {
		t.Fatalf("samples: got %d", got)
	}
	// 往返换算应无损（采样率整数倍）
	if got := SamplesToTime(960, 48000).Samples(48000); got != 960 {
		t.Fatalf("roundtrip: got %d", got)
	}
}
