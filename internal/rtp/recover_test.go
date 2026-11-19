package rtp

import (
	"testing"

	"avpipe/internal/fault"
)

func makePackets(n int, start uint16) []Packet {
	out := make([]Packet, n)
	for i := range out {
		out[i] = Packet{
			Seq: start + uint16(i), Timestamp: uint32(i * 960), SSRC: 0xDEAD,
			Payload: []byte{byte(i), byte(i >> 8), 0x55},
		}
	}
	return out
}

func TestExtendAcrossWraparound(t *testing.T) {
	if got := Extend(0xFFFF, 0xFFFE); got != 0xFFFF {
		t.Fatalf("got %x", got)
	}
	if got := Extend(0x0001, 0xFFFF); got != 0x10001 {
		t.Fatalf("wrap-forward got %x", got)
	}
	if got := Extend(0xFFFF, 0x10001); got != 0xFFFF {
		t.Fatalf("wrap-backward got %x", got)
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	p := Packet{Seq: 7, Timestamp: 12345, SSRC: 0xAABBCCDD, Marker: true, Payload: []byte{1, 2, 3}}
	buf, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != p.Seq || got.Timestamp != p.Timestamp || got.SSRC != p.SSRC || !got.Marker {
		t.Fatalf("header mismatch: %+v", got)
	}
	if string(got.Payload) != string(p.Payload) {
		t.Fatalf("payload=%v", got.Payload)
	}
}

func TestUnmarshalRejectsShortPacket(t *testing.T) {
	_, err := Unmarshal([]byte{0x80})
	if e := fault.Root(err); e == nil || e.Code != fault.CodeRTP {
		t.Fatalf("want rtp error, got %v", err)
	}
}

func TestRecoveryNoLoss(t *testing.T) {
	rec, err := SimulateReceive(makePackets(100, 1000), Config{Seed: 1, Window: 64})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Lost != 0 || rec.Rate != 1 {
		t.Fatalf("rec=%+v", rec)
	}
	for i := 1; i < len(rec.Ordered); i++ {
		if Extend(rec.Ordered[i].Seq, 1000) <= Extend(rec.Ordered[i-1].Seq, 1000) {
			t.Fatalf("not ordered at %d", i)
		}
	}
}

func TestRecoveryAcrossWraparound(t *testing.T) {
	pkts := makePackets(200, 0xFF00) // crosses 0xFFFF -> 0
	rec, err := SimulateReceive(pkts, Config{Seed: 9, Window: 64})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Ordered) != 200 || rec.Lost != 0 {
		t.Fatalf("wraparound lost packets: %+v", rec)
	}
	// The packet right after rollover must still sort last.
	if rec.Ordered[len(rec.Ordered)-1].Seq != 0xFFC7 {
		t.Fatalf("last seq=%x", rec.Ordered[len(rec.Ordered)-1].Seq)
	}
}

func TestDeterministicForSeed(t *testing.T) {
	pkts := makePackets(500, 10)
	a, _ := SimulateReceive(pkts, Config{Seed: 42, LossRate: 0.08, JitterMs: 35, Window: 32})
	b, _ := SimulateReceive(pkts, Config{Seed: 42, LossRate: 0.08, JitterMs: 35, Window: 32})
	if a.Lost != b.Lost || len(a.Ordered) != len(b.Ordered) || a.Conflicts != b.Conflicts {
		t.Fatalf("nondeterministic: %+v vs %+v", a, b)
	}
	for i := range a.Ordered {
		if a.Ordered[i].Seq != b.Ordered[i].Seq {
			t.Fatalf("order diverged at %d", i)
		}
	}
}

func TestRecoveryRateAtEightPercentLoss(t *testing.T) {
	pkts := makePackets(1000, 100)
	rec, err := SimulateReceive(pkts, Config{Seed: 42, LossRate: 0.08, JitterMs: 35, Window: 8})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Rate < 0.90 {
		t.Fatalf("recovery rate %.3f below 0.90 with window 8: %+v", rec.Rate, rec)
	}
}

func TestBeyondWindowGapIsUnrecoverable(t *testing.T) {
	pkts := makePackets(200, 0)
	// Deterministically remove a run of 40 packets, far beyond window 8.
	var kept []Packet
	for _, p := range pkts {
		if p.Seq >= 100 && p.Seq < 140 {
			continue
		}
		kept = append(kept, p)
	}
	rec, err := SimulateReceive(kept, Config{Seed: 1, Window: 8})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Unrecoverable != 40 {
		t.Fatalf("unrecoverable=%d want 40: %+v", rec.Unrecoverable, rec.Gaps)
	}
	if len(rec.Gaps) != 1 || rec.Gaps[0].Reason != ReasonWindow {
		t.Fatalf("gaps=%+v", rec.Gaps)
	}
	if rec.Rate >= 0.9 {
		t.Fatalf("rate %.3f should reflect unrecoverable gap", rec.Rate)
	}
}

func TestSmallGapIsRecoverable(t *testing.T) {
	pkts := makePackets(100, 0)
	var kept []Packet
	for _, p := range pkts {
		if p.Seq == 50 || p.Seq == 51 {
			continue
		}
		kept = append(kept, p)
	}
	rec, _ := SimulateReceive(kept, Config{Seed: 2, Window: 8})
	if rec.Unrecoverable != 0 || len(rec.Gaps) != 1 || rec.Gaps[0].Count != 2 {
		t.Fatalf("rec=%+v", rec)
	}
}

func TestDuplicateAndConflictDetection(t *testing.T) {
	pkts := makePackets(50, 0)
	rec, err := SimulateReceive(pkts, Config{Seed: 5, Window: 16, LatePackets: 10, ConflictCopy: 4})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Duplicates < 10 {
		t.Fatalf("duplicates=%d", rec.Duplicates)
	}
	if rec.Conflicts == 0 {
		t.Fatalf("conflicting duplicate not detected: %+v", rec)
	}
	if rec.Lost != 0 {
		t.Fatalf("duplicates counted as loss: %+v", rec)
	}
}

func TestLatePacketsDoNotReorderAcceptedStream(t *testing.T) {
	pkts := makePackets(20, 0)
	rec, _ := SimulateReceive(pkts, Config{Seed: 3, LatePackets: 5, Window: 8})
	for i := 1; i < len(rec.Ordered); i++ {
		if rec.Ordered[i-1].Seq+1 != rec.Ordered[i].Seq {
			t.Fatalf("late packet disturbed order at %d", i)
		}
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, err := SimulateReceive(nil, Config{LossRate: 1.5}); err == nil {
		t.Fatal("invalid loss accepted")
	}
	if _, err := SimulateReceive(nil, Config{JitterMs: -1}); err == nil {
		t.Fatal("negative jitter accepted")
	}
}

func TestMediaTicks(t *testing.T) {
	if got := MediaTicks(48000, 48000); got != 90000 {
		t.Fatalf("1s at 48k = %d", got)
	}
	if got := MediaTicks(9000, 90000); got != 9000 {
		t.Fatalf("0.1s video = %d", got)
	}
}
