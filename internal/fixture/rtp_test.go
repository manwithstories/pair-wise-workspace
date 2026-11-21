package fixture

import (
	"testing"

	"mediad/internal/rtp"
)

func TestPacketiseIsDeterministic(t *testing.T) {
	a := Packetise(17, 1, 10, 64, 90000, 3000, 65530, 1000)
	b := Packetise(17, 1, 10, 64, 90000, 3000, 65530, 1000)
	if len(a) != 10 {
		t.Fatalf("packets=%d", len(a))
	}
	for i := range a {
		if a[i].Packet.SequenceNumber != b[i].Packet.SequenceNumber {
			t.Fatalf("sequence differs at %d", i)
		}
		if string(a[i].Packet.Payload) != string(b[i].Packet.Payload) {
			t.Fatalf("payload differs at %d", i)
		}
	}
}

func TestFaultsProduceExpectedArrival(t *testing.T) {
	pkts := Packetise(17, 1, 20, 32, 90000, 3000, 0, 0)
	got, faults := ApplyFaults(pkts, []FaultKind{FaultDuplicate, FaultReorder, FaultBurstLoss}, FaultParams{
		DuplicateAt: []int{2, 3},
		ReorderAt:   []int{5, 6},
		BurstStart:  10,
		BurstLen:    3,
	})
	// 20 sent, 3 lost, 2 duplicated = 19 arrivals.
	if len(got) != 19 {
		t.Fatalf("arrivals=%d want 19", len(got))
	}
	// Packet 3 is both a duplicate target and the successor of the
	// reordered packet 2, so it is emitted once; the duplicate counter
	// reports what was actually injected.
	if faults != 6 {
		t.Fatalf("faults=%d want 6 (2 dup + 1 reorder + 3 loss)", faults)
	}

	r, _ := rtp.NewReceiver(rtp.Config{InitialWindow: 16, MinWindow: 4, MaxWindow: 64, ClockRate: 90000})
	var delivered []rtp.Packet
	for _, p := range got {
		d, err := r.Push(p)
		if err != nil {
			t.Fatal(err)
		}
		delivered = append(delivered, d...)
	}
	delivered = append(delivered, r.Flush()...)
	st := r.Stats()
	if st.Duplicates != 2 {
		t.Fatalf("duplicates=%d", st.Duplicates)
	}
	if st.Reordered != 1 {
		t.Fatalf("reordered=%d", st.Reordered)
	}
	if st.Lost != 3 {
		t.Fatalf("lost=%d", st.Lost)
	}
	if len(delivered) != 17 {
		t.Fatalf("delivered=%d want 17", len(delivered))
	}
	rep := rtp.ClassifyLoss(st, delivered, 4)
	if len(rep.Gaps) != 1 || rep.Gaps[0].Count != 3 {
		t.Fatalf("gaps=%+v", rep.Gaps)
	}
	if rep.RecoveredSamples != 3 {
		t.Fatalf("recovered=%d", rep.RecoveredSamples)
	}
}

func TestPayloadIndependentOfOrder(t *testing.T) {
	first := Payload(17, 1, 5, 64, nil)
	_ = Payload(17, 1, 0, 64, nil)
	again := Payload(17, 1, 5, 64, nil)
	if string(first) != string(again) {
		t.Fatal("payload depends on call order")
	}
	other := Payload(18, 1, 5, 64, nil)
	if string(first) == string(other) {
		t.Fatal("different seeds produced the same payload")
	}
}

func TestTimestampWrapFixtureSurvivesReceiver(t *testing.T) {
	// Start the 90kHz clock just before its 32-bit modulus so the stream
	// wraps once mid-run.
	const clockRate = 90000
	const startTS = uint32(0xFFFFFFFF - 60*3000)
	pkts := Packetise(17, 1, 120, 64, clockRate, clockRate/30, 0, startTS)
	got, faults := ApplyFaults(pkts, nil, FaultParams{})
	if faults != 0 {
		t.Fatalf("unexpected faults=%d", faults)
	}
	r, _ := rtp.NewReceiver(rtp.Config{InitialWindow: 16, MinWindow: 8, MaxWindow: 64, ClockRate: clockRate})
	var delivered []rtp.Packet
	for _, p := range got {
		d, err := r.Push(p)
		if err != nil {
			t.Fatal(err)
		}
		delivered = append(delivered, d...)
	}
	delivered = append(delivered, r.Flush()...)
	if len(delivered) != 120 {
		t.Fatalf("delivered=%d", len(delivered))
	}
	// The receiver preserves the wrap by design; the last packet's
	// timestamp is numerically smaller than the first's.
	if delivered[len(delivered)-1].Timestamp >= delivered[0].Timestamp {
		t.Fatal("expected the raw timestamps to wrap")
	}
}
