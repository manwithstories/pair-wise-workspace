package clock

import (
	"errors"
	"testing"
)

func TestNewBaseLCM(t *testing.T) {
	b, err := NewBase(90000, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if b.Timescale != 720000 {
		t.Fatalf("LCM(90000,48000)=%d, want 720000", b.Timescale)
	}
	if _, err := NewBase(0); err == nil {
		t.Fatal("want error for zero timescale")
	}
	if _, err := NewBase(-5); err == nil {
		t.Fatal("want error for negative timescale")
	}
}

func TestMapExact(t *testing.T) {
	b, _ := NewBase(90000, 48000)
	tick, err := b.Map(3000, 90000) // 1/30s
	if err != nil {
		t.Fatal(err)
	}
	if tick != 24000 {
		t.Fatalf("tick=%d, want 24000", tick)
	}
	if ms := b.Millis(24000); ms < 33.3 || ms > 33.4 {
		t.Fatalf("millis=%v, want ~33.33", ms)
	}
	// 非整除基准报错
	odd := Base{Timescale: 100}
	if _, err := odd.Map(1, 3); err == nil {
		t.Fatal("want divisibility error")
	}
}

func TestRationalNorm(t *testing.T) {
	r := Rational{6, -8}.Norm()
	if r.Num != -3 || r.Den != 4 {
		t.Fatalf("got %v", r)
	}
	if (Rational{0, 99}).Norm() != (Rational{0, 1}) {
		t.Fatal("zero norm failed")
	}
}

func TestTrackerWrap(t *testing.T) {
	b, _ := NewBase(90000)
	tr := NewTracker(b, 90000, 1<<32)
	if _, err := tr.Map(1<<32 - 1000); err != nil {
		t.Fatal(err)
	}
	// 回绕：从接近 2^32 跳到小值
	tick, err := tr.Map(2000)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(1<<32) + 2000 // 回绕后保持绝对延续
	if tick != want {
		t.Fatalf("tick=%d, want %d", tick, want)
	}
}

func TestTrackerRollback(t *testing.T) {
	b, _ := NewBase(90000)
	tr := NewTracker(b, 90000, 1<<32)
	if _, err := tr.Map(5000); err != nil {
		t.Fatal(err)
	}
	_, err := tr.Map(4000) // 小幅回退
	var rb *ErrRollback
	if !errors.As(err, &rb) {
		t.Fatalf("want ErrRollback, got %v", err)
	}
}

func TestTrackerReset(t *testing.T) {
	b, _ := NewBase(90000)
	tr := NewTracker(b, 90000, 1<<32)
	if _, err := tr.Map(90000); err != nil { // 1s
		t.Fatal(err)
	}
	tr.Reset(720000) // discontinuity：下一段从 1s 基准起算
	tick, err := tr.Map(0) // 断点后时间戳归零
	if err != nil {
		t.Fatal(err)
	}
	if tick != 720000 {
		t.Fatalf("tick=%d, want 720000", tick)
	}
	// 重置后小幅递增正常
	if _, err := tr.Map(3000); err != nil {
		t.Fatal(err)
	}
}
