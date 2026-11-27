package media

import (
	"errors"
	"math"
	"testing"
)

func TestExtend32(t *testing.T) {
	cases := []struct {
		name string
		prev int64
		in   uint32
		want int64
	}{
		{"steady", 1000, 1005, 1005},
		{"forward wrap", (1 << 32) - 2, 3, (1 << 32) + 3},
		{"backward near wrap", 1 << 32, 0xFFFFFFFE, (1 << 32) - 2},
		{"small reorder", 1 << 33, 0xFFFF0000, (1 << 32) + 0xFFFF0000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Extend32(c.prev, c.in); got != c.want {
				t.Fatalf("Extend32(%d,%#x)=%d want %d", c.prev, c.in, got, c.want)
			}
		})
	}
	// 连续递增越过回绕点必须单调。
	prev := int64(0xFFFFFFF0)
	for i := 0; i < 32; i++ {
		next := Extend32(prev, uint32(prev+1))
		if next != prev+1 {
			t.Fatalf("wrap walk broken at i=%d: %d -> %d", i, prev, next)
		}
		prev = next
	}
}

func TestScalePPM(t *testing.T) {
	// 150 ppm 漂移：每 1,000,000 tick 压缩 150。
	if got := ScalePPM(1_000_000, 150); got != 999_850 {
		t.Fatalf("ppm 150: got %d", got)
	}
	if got := ScalePPM(1_000_000, -150); got != 1_000_150 {
		t.Fatalf("ppm -150: got %d", got)
	}
	// 超出 ±200 ppm 必须钳制而不是继续放大。
	if got := ScalePPM(1_000_000, 500); got != ScalePPM(1_000_000, MaxPPM) {
		t.Fatalf("ppm clamp failed: %d", got)
	}
	// 负输入四舍五入不改变符号。
	if got := ScalePPM(-1_000_000, 150); got != -999_850 {
		t.Fatalf("negative ticks: got %d", got)
	}
}

func TestSeqDistance(t *testing.T) {
	cases := []struct {
		a, b uint16
		want int
	}{
		{10, 5, 5},
		{5, 10, -5},
		{0, 0xFFFF, 1},   // 回绕正向一步
		{0xFFFF, 0, -1},  // 反向一步
		{0x8000, 0, -32768},
	}
	for _, c := range cases {
		if got := SeqDistance(c.a, c.b); got != c.want {
			t.Fatalf("SeqDistance(%d,%d)=%d want %d", c.a, c.b, got, c.want)
		}
		if got := Wrap16(c.b, c.want); got != c.a {
			t.Fatalf("Wrap16 inverse failed: %d+%d=%d", c.b, c.want, got)
		}
	}
}

func TestMediaTimeConversions(t *testing.T) {
	m := New(48000, 48000)
	if m.ToNanos() != RateNanosec {
		t.Fatalf("1s at 48k should be 1e9 ns, got %d", m.ToNanos())
	}
	got := m.To(90000)
	if got.Ticks != 90000 {
		t.Fatalf("resample to 90kHz: got %d", got.Ticks)
	}
	a := New(2, 10) // 0.2s
	b := New(1, 10) // 0.1s
	if math.Abs(a.Sub(b)-0.1) > 1e-12 {
		t.Fatalf("Sub = %v", a.Sub(b))
	}
}

func TestFaultPropagation(t *testing.T) {
	f := NewFault(FaultBadPayload, 12000, 999, 77, "non-L16 marker")
	wrapped := errors.Join(errors.New("engine step"), f)
	got, ok := FindFault(wrapped)
	if !ok || got.Seq != 12000 || got.Offset != 999 || got.Kind != FaultBadPayload {
		t.Fatalf("fault not recovered: %+v ok=%v", got, ok)
	}
	if got.Error() == "" || got.Error()[:5] != "fault" {
		t.Fatalf("unexpected error text: %q", got.Error())
	}
	// 无故障的普通错误链返回 false。
	if _, ok := FindFault(errors.New("plain")); ok {
		t.Fatal("plain error must not match Fault")
	}
}

func TestFaultKindStableNames(t *testing.T) {
	want := map[FaultKind]string{
		FaultTruncated:  "truncated",
		FaultFraming:    "framing",
		FaultBadPayload: "bad_payload",
		FaultMissing:    "missing",
		FaultReordered:  "reordered",
		FaultLate:       "late",
		FaultDuplicate:  "duplicate",
		FaultWrap:       "timestamp_wrap",
	}
	for k, name := range want {
		if k.String() != name {
			t.Fatalf("kind %d -> %q want %q", k, k.String(), name)
		}
	}
}
