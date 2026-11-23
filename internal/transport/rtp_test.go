package transport

import (
	"testing"

	"github.com/pion/rtp"
)

func mkSeq(cfg Config, n int, stride uint16) []*rtp.Packet {
	// Build n packets whose payloads carry a redundant copy of the packet
	// `stride` positions earlier, like the sample generator does.
	var pkts []*rtp.Packet
	type rec struct {
		ts  uint32
		pl  []byte
	}
	var hist []rec
	for i := 0; i < n; i++ {
		pl := []byte{byte(i), byte(i >> 8), 0xAB}
		ts := uint32(i * 3000)
		full := pl
		if stride > 0 && i >= int(stride) {
			full = AppendRedundant(pl, hist[i-int(stride)].ts, hist[i-int(stride)].pl)
		}
		hist = append(hist, rec{ts, pl})
		pkts = append(pkts, &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: ts, SSRC: 7},
			Payload: full,
		})
	}
	_ = cfg
	return pkts
}

func feed(r *Reorderer, pkts []*rtp.Packet) []*rtp.Packet {
	var out []*rtp.Packet
	for _, p := range pkts {
		em, err := r.Push(p)
		if err != nil {
			panic(err)
		}
		out = append(out, em...)
	}
	return append(out, r.Drain()...)
}

func TestInOrderPassthrough(t *testing.T) {
	r := NewReorderer(Config{MaxQueue: 8, RedundancyStride: 4})
	out := feed(r, mkSeq(Config{}, 10, 4))
	if len(out) != 10 {
		t.Fatalf("got %d packets, want 10", len(out))
	}
	for i, p := range out {
		if p.SequenceNumber != uint16(i) {
			t.Fatalf("out[%d] seq=%d", i, p.SequenceNumber)
		}
	}
	if r.Stats().Lost != 0 {
		t.Fatalf("unexpected loss: %+v", r.Stats())
	}
}

func TestReorderBounded(t *testing.T) {
	pkts := mkSeq(Config{}, 6, 0)
	// Deliver 0,2,1,3,5,4.
	order := []int{0, 2, 1, 3, 5, 4}
	r := NewReorderer(Config{MaxQueue: 4})
	var out []*rtp.Packet
	for _, i := range order {
		em, err := r.Push(pkts[i])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, em...)
	}
	out = append(out, r.Drain()...)
	if len(out) != 6 {
		t.Fatalf("got %d, want 6", len(out))
	}
	for i, p := range out {
		if int(p.SequenceNumber) != i {
			t.Fatalf("out[%d] seq=%d", i, p.SequenceNumber)
		}
	}
}

func TestLossBurstRecovered(t *testing.T) {
	const stride = 8
	pkts := mkSeq(Config{}, 40, stride)
	// Drop a burst of 4 (seq 10..13); copies ride on seq 18..21.
	var kept []*rtp.Packet
	for _, p := range pkts {
		if p.SequenceNumber >= 10 && p.SequenceNumber < 14 {
			continue
		}
		kept = append(kept, p)
	}
	r := NewReorderer(Config{MaxQueue: 32, RedundancyStride: stride})
	out := feed(r, kept)
	if len(out) != 40 {
		t.Fatalf("got %d packets, want 40 (recovered)", len(out))
	}
	st := r.Stats()
	if st.Lost != 4 || st.Recovered != 4 {
		t.Fatalf("stats %+v, want lost=4 recovered=4", st)
	}
	if st.RecoveryRate() != 1.0 {
		t.Fatalf("rate %f", st.RecoveryRate())
	}
	// Recovered packets keep their original timestamps and payloads.
	for i := 10; i < 14; i++ {
		p := out[i]
		if p.Timestamp != uint32(i*3000) {
			t.Fatalf("recovered seq %d ts=%d", i, p.Timestamp)
		}
		if len(p.Payload) != 3 || p.Payload[0] != byte(i) {
			t.Fatalf("recovered seq %d payload %x", i, p.Payload)
		}
	}
}

func TestUnrecoverableGapNotFabricated(t *testing.T) {
	pkts := mkSeq(Config{}, 12, 0) // no redundancy at all
	var kept []*rtp.Packet
	for _, p := range pkts {
		if p.SequenceNumber == 5 || p.SequenceNumber == 6 {
			continue
		}
		kept = append(kept, p)
	}
	r := NewReorderer(Config{MaxQueue: 4})
	out := feed(r, kept)
	if len(out) != 10 {
		t.Fatalf("got %d, want 10", len(out))
	}
	gaps := r.Gaps()
	if len(gaps) != 1 || gaps[0].FromSeq != 5 || gaps[0].ToSeq != 6 {
		t.Fatalf("gaps %+v", gaps)
	}
	// Gap bounds come from neighbours: last emitted ts=4*3000, next=7*3000.
	if gaps[0].StartTS != 12000 || gaps[0].EndTS != 21000 {
		t.Fatalf("gap bounds %+v", gaps[0])
	}
	if r.Stats().RecoveryRate() != 0 {
		t.Fatalf("rate %f, want 0", r.Stats().RecoveryRate())
	}
}

func TestSequenceWrap(t *testing.T) {
	pkts := mkSeq(Config{}, 4, 0)
	for i, p := range pkts {
		p.SequenceNumber = uint16(65534 + i) // wraps past 65535
	}
	r := NewReorderer(Config{MaxQueue: 4})
	out := feed(r, []*rtp.Packet{pkts[0], pkts[2], pkts[1], pkts[3]})
	if len(out) != 4 {
		t.Fatalf("got %d, want 4", len(out))
	}
	for i, p := range out {
		if p.SequenceNumber != uint16(65534+i) {
			t.Fatalf("out[%d] seq=%d", i, p.SequenceNumber)
		}
	}
}

func TestQueueLimitRejected(t *testing.T) {
	pkts := mkSeq(Config{}, 10, 0)
	r := NewReorderer(Config{MaxQueue: 2})
	if _, err := r.Push(pkts[0]); err != nil {
		t.Fatal(err)
	}
	// seq 2 is within the window (distance 2 from newest=0).
	if _, err := r.Push(pkts[2]); err != nil {
		t.Fatal(err)
	}
	// seq 5 jumps 3 ahead of newest=2: beyond the window, rejected.
	if _, err := r.Push(pkts[5]); err == nil {
		t.Fatal("want queue limit error")
	}
}
