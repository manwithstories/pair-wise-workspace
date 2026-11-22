package rtp

import (
	"testing"
)

func TestCleanChannelRecoversNothing(t *testing.T) {
	obs := Build(50, Inject{DuplicateAt: -1})
	res, err := Recover(50, obs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Recovered != 0 || res.Unrecoverable != 0 || res.Duplicates != 0 || res.Rate != 0 {
		t.Fatalf("clean channel = %+v", res)
	}
	if len(res.Order) != 50 {
		t.Fatalf("ordered %d", len(res.Order))
	}
	for i, p := range res.Order {
		if p.SequenceNumber != uint16(i) {
			t.Fatalf("position %d holds seq %d", i, p.SequenceNumber)
		}
	}
}

func TestLossRecoveredByRetransmission(t *testing.T) {
	const n = 37
	obs := Build(n, Inject{DropEvery: 7, DuplicateAt: -1})
	res, err := Recover(n, obs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unrecoverable != 0 {
		t.Fatalf("retransmitted losses should be recoverable: %+v", res)
	}
	// With n=37 every 7th packet is dropped: 6, 13, 20, 27, 34.
	if want := 5; res.Recovered != want {
		t.Fatalf("recovered = %d, want %d: %+v", res.Recovered, want, res)
	}
	if res.Rate != 100 {
		t.Fatalf("recovery rate = %v, want 100", res.Rate)
	}
	// The rebuilt stream must be strictly ordered and complete.
	for i, p := range res.Order {
		if int(p.SequenceNumber) != i {
			t.Fatalf("ordered[%d].seq = %d", i, p.SequenceNumber)
		}
	}
}

func TestDuplicateDropped(t *testing.T) {
	obs := Build(10, Inject{DuplicateAt: 4})
	res, err := Recover(10, obs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicates != 1 || res.Unrecoverable != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Order) != 10 {
		t.Fatalf("ordered %d of 10", len(res.Order))
	}
}

func TestReorderRecovered(t *testing.T) {
	const n = 40
	obs := Build(n, Inject{ReorderEvery: 5})
	res, err := Recover(n, obs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reordered == 0 {
		t.Fatalf("swapped deliveries should be reported as reordered: %+v", res)
	}
	if res.Unrecoverable != 0 {
		t.Fatalf("nothing should be lost: %+v", res)
	}
}

func TestUnrecoverableCounted(t *testing.T) {
	// Build drops packets, then only keep the first-pass deliveries:
	// the retransmissions are cut, so those sequence numbers never
	// arrive.
	obs := Build(20, Inject{DropEvery: 5})
	var kept []Packet
	seen := map[uint16]bool{}
	for _, p := range obs {
		if seen[p.Seq] {
			kept = append(kept, p) // the duplicate is still a delivery
			continue
		}
		seen[p.Seq] = true
		kept = append(kept, p)
	}
	// Now genuinely lose a packet by removing its only delivery.
	var final []Packet
	for _, p := range kept {
		if p.Seq == 5 {
			continue
		}
		final = append(final, p)
	}
	res, err := Recover(20, final)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unrecoverable != 1 {
		t.Fatalf("unrecoverable = %d, want 1", res.Unrecoverable)
	}
	if got, want := res.Rate, float64(res.Recovered)*100/float64(res.Recovered+1); got != want {
		t.Fatalf("rate = %v, want %v", got, want)
	}
	if res.Reordered != 0 {
		t.Fatalf("no reordering was injected: %+v", res)
	}
}

func TestRejectsBadInput(t *testing.T) {
	if _, err := Recover(4, []Packet{{Seq: 9, RTP: newPacket(9)}}); err == nil {
		t.Fatal("out-of-range sequence must be rejected")
	}
	if _, err := Recover(4, []Packet{{Seq: 1}}); err == nil {
		t.Fatal("nil packet must be rejected")
	}
	if _, err := Recover(-1, nil); err == nil {
		t.Fatal("negative sent count must be rejected")
	}
}
