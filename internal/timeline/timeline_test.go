package timeline

import (
	"testing"
)

func TestClockRoundTrip(t *testing.T) {
	c, err := NewClock(1, KindVideo, 90000, 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := []int64{0, 1, 90000, 90001, 123456789}
	for _, tick := range cases {
		ns, _, err := c.ToNanos(tick)
		if err != nil {
			t.Fatalf("ToNanos(%d): %v", tick, err)
		}
		back, err := c.ToTick(ns)
		if err != nil {
			t.Fatalf("ToTick(%d): %v", ns, err)
		}
		// 纳秒反解 tick 误差不超过 1 个 tick。
		if back != tick && abs64(back-tick) > 1 {
			t.Fatalf("roundtrip tick=%d ns=%d back=%d", tick, ns, back)
		}
	}
}

func TestClockEpochAndRounding(t *testing.T) {
	c, _ := NewClock(7, KindAudio, 48000, 1_000_000_000)
	ns, errNs, err := c.ToNanos(48000)
	if err != nil {
		t.Fatal(err)
	}
	if ns != 2_000_000_000 {
		t.Fatalf("epoch mapping got %d", ns)
	}
	if errNs != 0 {
		t.Fatalf("exact tick should have zero error, got %d", errNs)
	}
	// 48000Hz 下 1 tick ≈ 20833.33ns，舍入误差必须有界。
	ns2, errNs2, _ := c.ToNanos(1)
	want := int64(1_000_000_000 + 20833)
	if ns2 != want || abs64(errNs2) > int64(NanoTimescale)/int64(48000) {
		t.Fatalf("rounding ns=%d err=%d", ns2, errNs2)
	}
}

func TestClockZeroTimescalePreservesRaw(t *testing.T) {
	c, _ := NewClock(3, KindEvent, 0, 0)
	_, _, err := c.ToNanos(555)
	me, ok := err.(*MapError)
	if !ok {
		t.Fatalf("want *MapError, got %T", err)
	}
	if me.TrackID != 3 || me.RawTick != 555 {
		t.Fatalf("raw identity lost: %+v", me)
	}
	if _, err := c.ToTick(10); err == nil {
		t.Fatal("zero timescale reverse map must fail")
	}
}

func TestClockTimescaleLimit(t *testing.T) {
	if _, err := NewClock(1, KindVideo, MaxTimescale+1, 0); err == nil {
		t.Fatal("oversized timescale must be rejected")
	}
}

func TestTopoSortAndCycle(t *testing.T) {
	g := NewGraph()
	mk := func(id uint32, kind string, deps ...uint32) *Track {
		c, _ := NewClock(id, kind, 1000, 0)
		return &Track{ID: id, Kind: kind, Clock: c, Deps: deps}
	}
	_ = g.Add(mk(3, KindEvent, 1, 2))
	_ = g.Add(mk(1, KindVideo))
	_ = g.Add(mk(2, KindAudio))
	order, err := g.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[uint32]int{}
	for i, tr := range order {
		pos[tr.ID] = i
	}
	if !(pos[1] < pos[3] && pos[2] < pos[3]) {
		t.Fatalf("deps not ordered before event: %v", order)
	}
	if got := g.Sorted()[0].Kind; got != KindVideo {
		t.Fatalf("stable sort wrong: %s", got)
	}

	// 循环依赖：1->2->3->1。
	g2 := NewGraph()
	_ = g2.Add(mk(1, KindVideo, 2))
	_ = g2.Add(mk(2, KindAudio, 3))
	_ = g2.Add(mk(3, KindEvent, 1))
	_, err = g2.TopoSort()
	ce, ok := err.(*CycleError)
	if !ok {
		t.Fatalf("want CycleError, got %v", err)
	}
	if len(ce.Path) < 4 || ce.Path[0] != ce.Path[len(ce.Path)-1] {
		t.Fatalf("cycle path malformed: %v", ce.Path)
	}
}

func TestMapperWrap(t *testing.T) {
	g := NewGraph()
	c, _ := NewClock(9, KindEvent, 1_000_000_000, 0) // 1 tick = 1ns
	_ = g.Add(&Track{ID: 9, Kind: KindEvent, Clock: c})
	m := NewMapper(g)
	// 先到接近 32 位边界的 tick，再发生回绕。
	p1, err := m.Map(9, RTPWrapMod-10, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := m.Map(9, 5, 1_000_000)
	if err != nil {
		t.Fatalf("wrap should be expanded, got %v", err)
	}
	if p2.Wrap != 1 || p2.Ns != int64(RTPWrapMod+5) {
		t.Fatalf("wrap expansion wrong: %+v", p2)
	}
	if p1.Ns >= p2.Ns {
		t.Fatal("timeline must remain monotonic across wrap")
	}
	// 原始 tick 必须保留回绕前的值。
	if p2.RawTick != 5 {
		t.Fatalf("raw tick not preserved: %d", p2.RawTick)
	}
}

func TestMapperRejectsBackStep(t *testing.T) {
	g := NewGraph()
	c, _ := NewClock(4, KindVideo, 1000, 0)
	_ = g.Add(&Track{ID: 4, Kind: KindVideo, Clock: c})
	m := NewMapper(g)
	if _, err := m.Map(4, 1000, 500); err != nil {
		t.Fatal(err)
	}
	_, err := m.Map(4, 900, 500) // 回退 100，小于阈值 => 真异常
	if err == nil {
		t.Fatal("small back-step must be reported, not wrapped")
	}
}

func TestMapperUnknownTrack(t *testing.T) {
	m := NewMapper(NewGraph())
	if _, err := m.Map(42, 1, 0); err == nil {
		t.Fatal("unknown track must fail mapping")
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
