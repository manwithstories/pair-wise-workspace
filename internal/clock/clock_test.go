package clock

import "testing"

func TestUnwrapNoWrap(t *testing.T) {
	var u Unwrapper
	seq := []uint64{100, 200, 300, 400}
	want := []int64{100, 200, 300, 400}
	for i, raw := range seq {
		if got := u.Unwrap(raw); got != want[i] {
			t.Fatalf("step %d: got %d want %d", i, got, want[i])
		}
	}
	if u.WrapCount() != 0 {
		t.Fatalf("unexpected wrap count %d", u.WrapCount())
	}
}

func TestUnwrapAcrossWrap(t *testing.T) {
	var u Unwrapper
	// 在 2^33 边界附近采样：差值应跨过回绕而不是倒退。
	base := uint64(Mod33 - 1800) // 边界前 20ms
	seq := []uint64{base, base + 900, base + 1800, 900, 1800}
	want := []int64{int64(base), int64(base) + 900, int64(base) + 1800, Mod33 + 900, Mod33 + 1800}
	for i, raw := range seq {
		if got := u.Unwrap(raw); got != want[i] {
			t.Fatalf("step %d: got %d want %d", i, got, want[i])
		}
	}
	if u.WrapCount() != 1 {
		t.Fatalf("wrap count = %d, want 1", u.WrapCount())
	}
}

func TestUnwrapDeterministic(t *testing.T) {
	raws := []uint64{5, uint64(Mod33 - 10), 20, 40, 60}
	run := func() []int64 {
		var u Unwrapper
		out := make([]int64, len(raws))
		for i, r := range raws {
			out[i] = u.Unwrap(uint64(r))
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("nondeterministic at %d: %d vs %d", i, a[i], b[i])
		}
	}
}

func TestFaultsHelpers(t *testing.T) {
	fs := Faults{
		{Kind: FaultCCGap, Offset: 376, PID: 0x101, Recovered: true},
		{Kind: FaultSyncLoss, Offset: 188, PID: 0x101},
		{Kind: FaultColorConflict, Offset: NoPosition, PID: NoPosition},
	}
	if got := fs.FirstOffset(); got != 188 {
		t.Fatalf("FirstOffset = %d, want 188", got)
	}
	if n := len(fs.Unrecovered()); n != 2 {
		t.Fatalf("Unrecovered len = %d, want 2", n)
	}
}

func TestMsRoundTrip(t *testing.T) {
	if got := MsToTicks(2000).Ms(); got != 2000 {
		t.Fatalf("Ms() = %d, want 2000", got)
	}
}
