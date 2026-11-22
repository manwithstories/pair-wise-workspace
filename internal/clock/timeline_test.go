package clock

import "testing"

func TestSamplesRoundTrip(t *testing.T) {
	for _, rate := range []int{8000, 44100, 48000} {
		for _, n := range []int64{0, 1, 1024, 48000, 1 << 20} {
			tk := SamplesToTicks(n, rate)
			got := TicksToSamples(tk, rate)
			// 允许 1 个采样的取整误差
			if d := got - n; d < -1 || d > 1 {
				t.Fatalf("rate=%d n=%d roundtrip=%d", rate, n, got)
			}
		}
	}
}

func TestTimelineDiscontinuity(t *testing.T) {
	tl := NewTimeline()
	// discontinuity：本地 10s 处显式重置到统一时钟 30s
	if err := tl.Reset(10*Second, 30*Second); err != nil {
		t.Fatal(err)
	}
	u, err := tl.Map(12 * Second)
	if err != nil {
		t.Fatal(err)
	}
	if u != 32*Ticks(Second) {
		t.Fatalf("got %d want %d", u, 32*Second)
	}
	// 重置点之前仍走旧映射
	u, _ = tl.Map(5 * Second)
	if u != 5*Ticks(Second) {
		t.Fatalf("got %d want %d", u, 5*Second)
	}
}

func TestTimelineAmbiguousReset(t *testing.T) {
	tl := NewTimeline()
	if err := tl.Reset(10*Second, 30*Second); err != nil {
		t.Fatal(err)
	}
	// 在更早的本地时间再次重置 → 无歧义映射不可能
	if err := tl.Reset(5*Second, 40*Second); err == nil {
		t.Fatal("expected ambiguity error")
	}
}

func TestNegativeLocal(t *testing.T) {
	tl := NewTimeline()
	if _, err := tl.Map(-1); err == nil {
		t.Fatal("expected error for negative local time")
	}
}
