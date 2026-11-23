package clock

import (
	"errors"
	"testing"
)

func mustMapper(t *testing.T, tb Timebase, epoch int64) *Mapper {
	t.Helper()
	m, err := NewMapper(tb)
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if err := m.SetEpochUnits(epoch); err != nil {
		t.Fatalf("SetEpochUnits: %v", err)
	}
	return m
}

func TestMapIsMonotonicAtSteadyCadence(t *testing.T) {
	tb := Timebase{Timescale: 90000, CaptureUPS: 1000} // 1 ms capture
	const epoch = 1_700_000_000_000
	m := mustMapper(t, tb, epoch)
	var prev int64 = -1
	for i := int64(0); i < 240; i++ {
		pts, ev, err := m.Map(epoch + i)
		if err != nil {
			t.Fatalf("Map(%d): %v", epoch+i, err)
		}
		if pts <= prev {
			t.Fatalf("frame %d: pts %d not greater than %d", i, pts, prev)
		}
		if ev != EventNone {
			t.Fatalf("frame %d: unexpected repair %s", i, ev)
		}
		prev = pts
	}
	// 1 ms at 90 kHz is exactly 90 ticks.
	if prev != 239*90 {
		t.Fatalf("last pts = %d, want %d", prev, 239*90)
	}
	if got := m.Repairs(); got != (Repairs{}) {
		t.Fatalf("unexpected repairs %+v", got)
	}
}

func TestBackwardsJitterClamped(t *testing.T) {
	tb := Timebase{Timescale: 90000, CaptureUPS: 1000}
	const epoch = 1_700_000_000_000
	m := mustMapper(t, tb, epoch)
	if _, _, err := m.Map(epoch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Map(epoch + 10); err != nil {
		t.Fatal(err)
	}
	// 1 ms backwards: within tolerance, pinned to the previous PTS.
	pts, ev, err := m.Map(epoch + 9)
	if err != nil {
		t.Fatal(err)
	}
	if ev != EventClamp {
		t.Fatalf("event = %s, want clamp", ev)
	}
	if got := m.Repairs().Clamps; got != 1 {
		t.Fatalf("clamps = %d, want 1", got)
	}
	if pts != 10*90 {
		t.Fatalf("pts = %d, want %d", pts, 10*90)
	}
}

func TestLargeRollbackRepairedWithOffset(t *testing.T) {
	tb := Timebase{Timescale: 90000, CaptureUPS: 1000}
	const epoch = 1_700_000_000_000
	m := mustMapper(t, tb, epoch)
	for i := int64(0); i < 5; i++ {
		if _, _, err := m.Map(epoch + i); err != nil {
			t.Fatal(err)
		}
	}
	// Device clock jumps back 2 seconds; cadence after must be preserved.
	pts, ev, err := m.Map(epoch + 5 - 2000)
	if err != nil {
		t.Fatal(err)
	}
	if ev != EventRollback {
		t.Fatalf("event = %s, want rollback", ev)
	}
	if m.Repairs().Rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1", m.Repairs().Rollbacks)
	}
	// The jumped frame must continue the cadence: last good PTS was 4 ms,
	// so this one lands at 5 ms.
	if pts != 5*90 {
		t.Fatalf("pts = %d, want %d", pts, 5*90)
	}
	// Next frame is exactly one ms later, proving the offset held.
	next, _, err := m.Map(epoch + 6 - 2000)
	if err != nil {
		t.Fatal(err)
	}
	if next-pts != 90 {
		t.Fatalf("cadence after rollback = %d ticks, want 90", next-pts)
	}
}

func TestWrapArithmetic(t *testing.T) {
	// 32-bit counter at 90 kHz wraps every ~13 hours; test the seam.
	tb := Timebase{Timescale: 90000, CaptureUPS: 90000, WrapUnits: 1 << 32}
	m := mustMapper(t, tb, 0)
	near := int64(1<<32) - 3
	var prev int64 = -1
	for _, raw := range []int64{near, near + 1, near + 2, 0, 1, 2} {
		pts, ev, err := m.Map(raw)
		if err != nil {
			t.Fatalf("Map(%d): %v", raw, err)
		}
		if pts <= prev {
			t.Fatalf("raw %d: pts %d not monotonic (prev %d)", raw, pts, prev)
		}
		prev = pts
		_ = ev
	}
	if m.Repairs().Wraps != 1 {
		t.Fatalf("wraps = %d, want 1", m.Repairs().Wraps)
	}
	if prev != 1<<32+2 {
		t.Fatalf("final pts = %d, want %d", prev, uint64(1)<<32+2)
	}
}

func TestMixedPrecisionSameInstant(t *testing.T) {
	// Two devices quantise onto the same millisecond.
	tb := Timebase{Timescale: 1000, CaptureUPS: 1000}
	const epoch = 1_700_000_000_000
	m := mustMapper(t, tb, epoch)
	if _, _, err := m.Map(epoch); err != nil {
		t.Fatal(err)
	}
	pts2, ev, err := m.Map(epoch) // identical instant, no advancement
	if err != nil {
		t.Fatal(err)
	}
	if pts2 != 0 {
		t.Fatalf("pts = %d, want 0", pts2)
	}
	if ev != EventSameTime {
		t.Fatalf("event = %s, want same_time", ev)
	}
}

func TestTimebaseUnitsToTicks(t *testing.T) {
	tb := Timebase{Timescale: 90000, CaptureUPS: 30000}
	if got := tb.UnitsToTicks(1); got != 3 {
		t.Fatalf("UnitsToTicks(1) = %d, want 3", got)
	}
	if got := tb.UnitsToTicks(30000); got != 90000 {
		t.Fatalf("UnitsToTicks(30000) = %d, want 90000", got)
	}
	if got := tb.TickQuantum(); got != 3 {
		t.Fatalf("TickQuantum = %d, want 3", got)
	}
}

func TestValidateRejectsBadTimebase(t *testing.T) {
	for _, tb := range []Timebase{
		{CaptureUPS: 1000},
		{Timescale: 90000},
		{Timescale: 90000, CaptureUPS: 1000, WrapUnits: -1},
	} {
		if _, err := NewMapper(tb); !errors.Is(err, ErrBadTimebase) {
			t.Fatalf("tb %+v: err = %v, want ErrBadTimebase", tb, err)
		}
	}
}

func TestSequencerClassifies(t *testing.T) {
	s := &Sequencer{}
	h := func(b byte) [32]byte { var x [32]byte; x[0] = b; return x }
	cases := []struct {
		no   uint64
		hash byte
		want SeqStatus
	}{
		{1, 1, SeqOK},
		{2, 2, SeqOK},
		{2, 2, SeqDuplicate},
		{2, 3, SeqConflict},
		{1, 4, SeqOutOfOrder},
		{3, 5, SeqOK},
	}
	for i, c := range cases {
		if got := s.Accept(c.no, h(c.hash)); got != c.want {
			t.Fatalf("case %d: got %s, want %s", i, got, c.want)
		}
	}
	dup, conf := s.Counts()
	if dup != 1 || conf != 2 {
		t.Fatalf("dup=%d conflict=%d, want 1/2", dup, conf)
	}
}
