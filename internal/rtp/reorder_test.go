package rtp

import (
	"fmt"
	"testing"

	pionrtp "github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/model"
)

func TestReordersAndDeduplicates(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InitialWindow = 8
	r, err := NewReceiver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var in []*pionrtp.Packet
	for i := 0; i < 10; i++ {
		in = append(in, packet(uint16(i), uint32(i*3000)))
	}
	// Put them down in a scrambled order with one duplicate.
	order := []int{0, 2, 1, 3, 5, 4, 6, 6, 7, 9, 8}
	var pkts []*pionrtp.Packet
	for _, i := range order {
		pkts = append(pkts, in[i])
	}
	out := feed(t, r, pkts)
	out = append(out, r.Flush()...)

	if len(out) != 10 {
		t.Fatalf("delivered %d want 10", len(out))
	}
	for i, p := range out {
		if p.Seq != uint64(i) {
			t.Fatalf("position %d has seq %d", i, p.Seq)
		}
	}
	st := r.Stats()
	if st.Duplicates != 1 {
		t.Fatalf("duplicates=%d", st.Duplicates)
	}
	if st.Reordered != 3 {
		t.Fatalf("reordered=%d want 3", st.Reordered)
	}
	if st.Lost != 0 {
		t.Fatalf("lost=%d", st.Lost)
	}
}

func TestSequenceWrap(t *testing.T) {
	r, _ := NewReceiver(DefaultConfig())
	var pkts []*pionrtp.Packet
	for i := 65530; i <= 65535; i++ {
		pkts = append(pkts, packet(uint16(i), uint32(i)))
	}
	for i := 0; i < 5; i++ {
		pkts = append(pkts, packet(uint16(i), uint32(i)))
	}
	out := feed(t, r, pkts)
	out = append(out, r.Flush()...)
	if len(out) != 11 {
		t.Fatalf("delivered %d want 11", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].Seq != out[i-1].Seq+1 {
			t.Fatalf("non-consecutive at %d: %d then %d", i, out[i-1].Seq, out[i].Seq)
		}
	}
	if r.Stats().Lost != 0 || r.Stats().Duplicates != 0 {
		t.Fatalf("unexpected loss across wrap: %+v", r.Stats())
	}
}

func TestBurstLossIsReportedNotInvented(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InitialWindow = 4
	cfg.MinWindow = 2
	r, _ := NewReceiver(cfg)
	var pkts []*pionrtp.Packet
	for i := 0; i < 20; i++ {
		if i >= 8 && i < 12 {
			continue // burst of 4 lost
		}
		pkts = append(pkts, packet(uint16(i), uint32(i*3000)))
	}
	out := feed(t, r, pkts)
	out = append(out, r.Flush()...)
	if len(out) != 16 {
		t.Fatalf("delivered %d want 16", len(out))
	}
	st := r.Stats()
	if st.Lost != 4 {
		t.Fatalf("lost=%d want 4", st.Lost)
	}
	rep := ClassifyLoss(st, out, 8)
	if len(rep.Gaps) != 1 {
		t.Fatalf("gaps=%d want 1", len(rep.Gaps))
	}
	g := rep.Gaps[0]
	if g.Count != 4 || g.FirstSeq != 8 || g.LastSeq != 11 {
		t.Fatalf("gap=%+v", g)
	}
	if g.StartTicks != 7*3000 || g.EndTicks != 12*3000 {
		t.Fatalf("gap bounds %d..%d", g.StartTicks, g.EndTicks)
	}
	if !g.Recoverable {
		t.Fatal("short bounded gap should be recoverable")
	}

	// A burst longer than the recovery limit stays unrecoverable.
	rep2 := ClassifyLoss(st, out, 2)
	if rep2.Gaps[0].Recoverable {
		t.Fatal("long burst must not be recoverable")
	}
	if rep2.UnrecoverableSamples != 4 {
		t.Fatalf("unrecoverable=%d", rep2.UnrecoverableSamples)
	}
}

func TestWindowAdaptsToBandwidth(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InitialWindow = 64
	cfg.MinWindow = 8
	cfg.MaxWindow = 512
	cfg.ReferenceBandwidthMbps = 8
	r, _ := NewReceiver(cfg)
	if r.Window() != 64 {
		t.Fatalf("initial window=%d", r.Window())
	}
	r.SetBandwidth(2)
	if r.Window() != 256 {
		t.Fatalf("at 2Mbps window=%d want 256", r.Window())
	}
	r.SetBandwidth(0.5)
	if r.Window() != 512 {
		t.Fatalf("at 0.5Mbps window=%d want 512 (capped)", r.Window())
	}
	r.SetBandwidth(100)
	if r.Window() != 8 {
		t.Fatalf("at 100Mbps window=%d want 8 (floor)", r.Window())
	}
	shifts := r.Stats().WindowShifts
	if len(shifts) != 3 {
		t.Fatalf("shifts=%d want 3", len(shifts))
	}
	for _, s := range shifts {
		if s.To < cfg.MinWindow || s.To > cfg.MaxWindow {
			t.Fatalf("shift out of bounds: %+v", s)
		}
	}
}

func TestLatePacketAfterLossIsCounted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InitialWindow = 2
	cfg.MinWindow = 1
	r, _ := NewReceiver(cfg)
	feed(t, r, []*pionrtp.Packet{
		packet(0, 0), packet(1, 3000), packet(2, 6000),
		packet(6, 18000), packet(7, 21000),
	})
	// The window has advanced past 3..5; they are now too late.
	got, _ := r.Push(packet(3, 9000))
	if len(got) != 0 {
		t.Fatalf("late packet was delivered: %+v", got)
	}
	st := r.Stats()
	if st.TooLate != 1 {
		t.Fatalf("too_late=%d", st.TooLate)
	}
	if st.Lost != 3 {
		t.Fatalf("lost=%d want 3", st.Lost)
	}
}

func TestBufferLimitIsAnError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InitialWindow = 64
	cfg.MinWindow = 64
	cfg.MaxWindow = 64
	cfg.MaxBuffered = 4
	r, _ := NewReceiver(cfg)
	// Anchor at 0, then hold a gap at 1 while pushing later packets. The
	// window is far wider than the buffer, so the receiver must refuse the
	// fifth buffered packet instead of silently discarding it.
	if _, err := r.Push(packet(0, 0)); err != nil {
		t.Fatal(err)
	}
	var err error
	for i := 2; i <= 12 && err == nil; i++ {
		_, err = r.Push(packet(uint16(i), uint32(i)))
	}
	if err == nil {
		t.Fatal("expected buffer limit error")
	}
	if model.KindOf(err) != model.KindOverLimit {
		t.Fatalf("kind=%v", model.KindOf(err))
	}
}

func TestInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinWindow = 100
	cfg.MaxWindow = 10
	if _, err := NewReceiver(cfg); err == nil {
		t.Fatal("expected config error")
	}
}

func TestTrackCarriesRawTimestamps(t *testing.T) {
	r, _ := NewReceiver(DefaultConfig())
	var pkts []*pionrtp.Packet
	// Timestamps near the 32-bit wrap.
	base := uint32(0xFFFFFF00)
	for i := 0; i < 8; i++ {
		pkts = append(pkts, packet(uint16(i), base+uint32(i)*3000))
	}
	out := feed(t, r, pkts)
	tr, err := r.Track(1, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Samples) != 8 {
		t.Fatalf("samples=%d", len(tr.Samples))
	}
	// The track must expose the wrap rather than smoothing it away, so the
	// repair stage can count it.
	nonMonotonic := false
	for i := 1; i < len(tr.Samples); i++ {
		if !tr.Samples[i-1].DTS.Less(tr.Samples[i].DTS) {
			nonMonotonic = true
		}
	}
	if !nonMonotonic {
		t.Fatal("wrap was hidden by the receiver")
	}
}

func mustRat(n, d int64) clock.Rational {
	x, err := clock.New(n, d)
	if err != nil {
		panic(err)
	}
	return x
}

func TestStatsSnapshotIsACopy(t *testing.T) {
	r, _ := NewReceiver(DefaultConfig())
	feed(t, r, []*pionrtp.Packet{packet(0, 0), packet(5, 1)})
	r.Flush()
	a := r.Stats()
	a.MissingSeqs = append(a.MissingSeqs, 999)
	b := r.Stats()
	if len(b.MissingSeqs) != 4 {
		t.Fatalf("snapshot aliased: %v", b.MissingSeqs)
	}
	_ = fmt.Sprint(b)
}
