package clock

import (
	"errors"
	"testing"
)

func TestTicksToNanos(t *testing.T) {
	if got := TicksToNanos(1000, 1_000_000, 1); got != Nanos(1_000_000_000) {
		t.Fatalf("1s in ms ticks = %d", got)
	}
	// 90kHz RTP 时钟: 90000 刻度 = 1s
	if got := TicksToNanos(90000, 1_000_000_000, 90000); got != Second {
		t.Fatalf("90kHz 1s = %d", got)
	}
	// 大刻度不溢出
	if got := TicksToNanos(1<<40, 1_000_000_000, 90000); got <= 0 {
		t.Fatalf("overflow: %d", got)
	}
	if got := TicksToNanos(5, 1, 0); got != 0 {
		t.Fatalf("zero den = %d", got)
	}
}

func TestUnwrapper(t *testing.T) {
	var u Unwrapper
	if got := u.Extend(0xFFFFFFF0); got != 0xFFFFFFF0 {
		t.Fatalf("init = %d", got)
	}
	// 正向回绕
	if got := u.Extend(0x00000010); got != 0x100000010 {
		t.Fatalf("wrap forward = %#x", got)
	}
	// 乱序旧包（回绕前）
	if got := u.Extend(0xFFFFFFF8); got != 0xFFFFFFF8 {
		t.Fatalf("late pre-wrap = %#x", got)
	}
	// 继续前进
	if got := u.Extend(0x00000020); got != 0x100000020 {
		t.Fatalf("continue = %#x", got)
	}
}

func TestTimelineValidate(t *testing.T) {
	tl := NewTimeline()
	if err := tl.Validate(1, 100); err != nil {
		t.Fatal(err)
	}
	if err := tl.Validate(1, 200); err != nil {
		t.Fatal(err)
	}
	// 另一轨道独立
	if err := tl.Validate(2, 50); err != nil {
		t.Fatal(err)
	}
	// 非单调
	err := tl.Validate(1, 150)
	var se *StampError
	if !errors.As(err, &se) || se.Kind != ErrNonMonotonic || se.Prev != 200 || se.Value != 150 {
		t.Fatalf("non-monotonic: %v", err)
	}
	// 负时间戳
	err = tl.Validate(2, -5)
	if !errors.As(err, &se) || se.Kind != ErrNegativeTimestamp || se.Value != -5 {
		t.Fatalf("negative: %v", err)
	}
	// 违规值不进入基准：后续 250 仍相对 200 判定
	if err := tl.Validate(1, 250); err != nil {
		t.Fatal(err)
	}
}
