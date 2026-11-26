package rtp

import (
	"testing"

	"mediamix/internal/clock"
)

func TestParseRejectsInvalidLengths(t *testing.T) {
	bad := [][]byte{
		nil,
		make([]byte, 11),
		{0x80, 0x8B, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0xFF}, // padding flag, 1 pad byte claims more than available
	}
	bad[2][0] = 0xA0
	for i, b := range bad {
		if _, err := Parse(b); err == nil {
			t.Fatalf("case %d: expected parse error", i)
		} else if !clock.IsCode(err, clock.ErrInvalidPacket) {
			t.Fatalf("case %d: expected ErrInvalidPacket, got %v", i, err)
		}
	}
}

func TestMarshalParseRoundTrip(t *testing.T) {
	p := &Packet{Version: 2, PayloadType: 96, Seq: 4242, Timestamp: 777, SSRC: 0xDEADBEEF, Payload: []byte{1, 2, 3, 4, 5}}
	got, err := Parse(p.Marshal())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Seq != p.Seq || got.Timestamp != p.Timestamp || got.SSRC != p.SSRC || string(got.Payload) != string(p.Payload) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func pushAll(t *testing.T, r *Reorderer, pkts []*Packet) []*Packet {
	t.Helper()
	var out []*Packet
	for _, p := range pkts {
		rel, _ := r.Push(p)
		out = append(out, rel...)
	}
	return append(out, r.Flush()...)
}

func TestReorderAndDuplicate(t *testing.T) {
	fx := Generate(DefaultStreamConfig(), FixtureConfig{Seed: 1, Seconds: 2, ReorderRate: 0.5, DuplicateRate: 0.2, SenderReportEvery: 50})
	r := NewReorderer(DefaultReorderConfig())
	out := pushAll(t, r, fx.Packets)
	for i := 1; i < len(out); i++ {
		if clock.Wrap16(out[i-1].Seq, out[i].Seq) != 1 {
			t.Fatalf("output not in sequence order at %d: %d -> %d", i, out[i-1].Seq, out[i].Seq)
		}
	}
	st := r.Stats()
	if st.Reordered == 0 {
		t.Fatal("expected reorder events")
	}
	if st.Duplicates == 0 {
		t.Fatal("expected duplicate events")
	}
}

func TestLossDetection(t *testing.T) {
	fx := Generate(DefaultStreamConfig(), FixtureConfig{Seed: 2, Seconds: 3, LossRate: 0.1, SenderReportEvery: 50})
	injected := 0
	for _, e := range fx.Events {
		if e.Kind == "loss" {
			injected++
		}
	}
	r := NewReorderer(DefaultReorderConfig())
	out := pushAll(t, r, fx.Packets)
	if r.Stats().Lost != injected {
		t.Fatalf("lost=%d want %d", r.Stats().Lost, injected)
	}
	if len(out) != int(fx.TotalSeq)-injected {
		t.Fatalf("delivered=%d want %d", len(out), int(fx.TotalSeq)-injected)
	}
}

func TestSequenceWrap(t *testing.T) {
	fx := Generate(DefaultStreamConfig(), FixtureConfig{Seed: 3, Seconds: 4, LossRate: 0.05, ForceSeqWrap: true, SenderReportEvery: 50})
	r := NewReorderer(DefaultReorderConfig())
	out := pushAll(t, r, fx.Packets)
	for i := 1; i < len(out); i++ {
		// Gaps from losses are allowed; backward steps or duplicates are not.
		if d := clock.Wrap16(out[i-1].Seq, out[i].Seq); d <= 0 {
			t.Fatalf("wrap ordering broken at %d: %d -> %d (d=%d)", i, out[i-1].Seq, out[i].Seq, d)
		}
	}
	if r.Stats().Wrapped == 0 {
		t.Fatal("expected at least one wrap event")
	}
	if r.Stats().Lost == 0 {
		t.Fatal("expected injected losses to be detected")
	}
}

func TestTimestampWrap(t *testing.T) {
	fx := Generate(DefaultStreamConfig(), FixtureConfig{Seed: 4, Seconds: 3, ForceTimestampWrap: true, SenderReportEvery: 40})
	// Timestamp monotonicity must hold in modular space even across 2^32.
	first, last := fx.Packets[0], fx.Packets[len(fx.Packets)-1]
	if clock.Wrap32(first.Timestamp, last.Timestamp) <= 0 {
		t.Fatal("timestamp did not advance across wrap")
	}
	sawWrap := false
	var prev uint32
	for i, p := range fx.Packets {
		if i > 0 && clock.Wrap32(prev, p.Timestamp) < 0 {
			t.Fatal("non-monotonic timestamp in fixture")
		}
		if p.Timestamp < prev {
			sawWrap = true
		}
		prev = p.Timestamp
	}
	if !sawWrap {
		t.Fatal("fixture did not cross the 32-bit timestamp boundary")
	}
}

func TestSRTickRoundTripAndCalibration(t *testing.T) {
	sr := SenderReport{SSRC: 1, NTPTick: clock.FromTime(2500_000_000), RTPTimestamp: 40000, PacketCount: 10}
	parsed, err := ParseSR(MarshalSR(sr))
	if err != nil {
		t.Fatalf("parse sr: %v", err)
	}
	if parsed.NTPTick != sr.NTPTick || parsed.RTPTimestamp != sr.RTPTimestamp {
		t.Fatalf("SR round trip mismatch: %+v", parsed)
	}
	c := NewCalibrator(16000)
	if err := c.Apply(parsed); err != nil {
		t.Fatalf("apply: %v", err)
	}
	tick, err := c.ToTick(56000) // +16000 samples = +1 s at 16 kHz
	if err != nil {
		t.Fatalf("totick: %v", err)
	}
	if tick != sr.NTPTick+clock.FromTime(1_000_000_000) {
		t.Fatalf("calibrated tick drift: got %d", tick)
	}
}

func TestRejectedReportRetainsMapping(t *testing.T) {
	c := NewCalibrator(16000)
	first := &SenderReport{RTPTimestamp: 16000, NTPTick: clock.FromTime(1_000_000_000)}
	if err := c.Apply(first); err != nil {
		t.Fatal(err)
	}
	good, _ := c.ToTick(32000)
	// Jump 3 seconds ahead of where the mapping predicts.
	bad := &SenderReport{RTPTimestamp: 32000, NTPTick: clock.FromTime(4_000_000_000)}
	if err := c.Apply(bad); !clock.IsCode(err, clock.ErrReportJump) {
		t.Fatalf("expected jump rejection, got %v", err)
	}
	again, _ := c.ToTick(32000)
	if again != good {
		t.Fatalf("mapping changed after rejected report: %d != %d", again, good)
	}
	if c.Rejected != 1 || c.Reports != 1 {
		t.Fatalf("calibrator counters wrong: %+v", c)
	}
}

func TestParseSRRejectsGarbage(t *testing.T) {
	if _, err := ParseSR(make([]byte, 10)); !clock.IsCode(err, clock.ErrInvalidPacket) {
		t.Fatalf("expected invalid packet, got %v", err)
	}
}
