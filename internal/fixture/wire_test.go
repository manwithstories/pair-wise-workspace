package fixture

import (
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/transport"
)

func payloadOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 31)
	}
	return b
}

func TestWireInOrderNoLoss(t *testing.T) {
	w, err := BuildWire(41, payloadOf(48000*2), 2.0, WireConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fault.Set{}
	res := transport.NewReorderer(4).Process(w.Packets, fs)
	if res.Duplicates != 0 || res.Lost != 0 || len(res.Gaps) != 0 || res.Reordered != 0 {
		t.Fatalf("res=%+v", res)
	}
	// Reassembled payload must equal the original exactly.
	if string(res.Reassemble()) != string(w.Payload) {
		t.Fatalf("payload mismatch: %d vs %d bytes", len(res.Reassemble()), len(w.Payload))
	}
}

func TestWireBurstLossAndGaps(t *testing.T) {
	w, err := BuildWire(41, payloadOf(48000*2), 2.0, WireConfig{LossBurst: 3, LossAt: 10})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fault.Set{}
	res := transport.NewReorderer(4).Process(w.Packets, fs)
	if len(res.Gaps) != 1 || res.Gaps[0].Len() != 3 {
		t.Fatalf("gaps=%+v", res.Gaps)
	}
	if res.Lost != 3 || fs.Count(fault.KindPacketGap) != 1 {
		t.Fatalf("lost=%d faults=%v", res.Lost, fs.Faults())
	}
	// No fabricated frames: surviving packet count is Sent-3.
	if len(res.Packets) != w.Sent-3 {
		t.Fatalf("packets=%d want %d", len(res.Packets), w.Sent-3)
	}
}

func TestWireDuplicatesAndReorder(t *testing.T) {
	w, err := BuildWire(41, payloadOf(48000*2), 2.0, WireConfig{Duplicate: 2, Reorder: 5})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fault.Set{}
	res := transport.NewReorderer(8).Process(w.Packets, fs)
	if res.Duplicates != 2 {
		t.Fatalf("duplicates=%d", res.Duplicates)
	}
	if res.Reordered == 0 {
		t.Fatal("expected reorder detection")
	}
	if len(res.Packets) != w.Sent {
		t.Fatalf("packets=%d want %d", len(res.Packets), w.Sent)
	}
	if res.Lost != 0 || len(res.Gaps) != 0 {
		t.Fatalf("unexpected loss: %+v", res.Gaps)
	}
}

func TestWireDeterministic(t *testing.T) {
	cfg := WireConfig{LossBurst: 3, Reorder: 5, Duplicate: 2, BandwidthMbps: []float64{2, 0.4, 4}}
	a, _ := BuildWire(41, payloadOf(96000), 2.0, cfg)
	b, _ := BuildWire(41, payloadOf(96000), 2.0, cfg)
	if len(a.Packets) != len(b.Packets) {
		t.Fatalf("packet counts differ")
	}
	for i := range a.Packets {
		if a.Packets[i].Seq != b.Packets[i].Seq {
			t.Fatalf("packet %d seq differs", i)
		}
	}
	if len(a.Bandwidth) != len(b.Bandwidth) {
		t.Fatal("bandwidth series differs")
	}
}

func TestWireBandwidthSeries(t *testing.T) {
	w, _ := BuildWire(41, payloadOf(96000), 2.0, WireConfig{BandwidthMbps: []float64{2, 0.4, 4}})
	if len(w.Bandwidth) < 3 {
		t.Fatalf("samples=%d", len(w.Bandwidth))
	}
	// Values cycle through the series.
	if w.Bandwidth[0].Bps != 2_000_000 || w.Bandwidth[1].Bps != 400_000 || w.Bandwidth[2].Bps != 4_000_000 {
		t.Fatalf("bandwidth=%+v", w.Bandwidth)
	}
}

func TestWireRejectsBadInput(t *testing.T) {
	if _, err := BuildWire(1, nil, 1, WireConfig{}); err == nil {
		t.Fatal("empty payload accepted")
	}
	if _, err := BuildWire(1, payloadOf(10), 0, WireConfig{}); err == nil {
		t.Fatal("zero duration accepted")
	}
}
