package rtp

import (
	"fmt"
	"testing"
	"time"
)

func TestReorderWrapDuplicatesAndLate(t *testing.T) {
	r := NewReorderer()
	start := uint16(65530)
	var got []uint16
	now := time.Duration(0)
	order := []int{0, 2, 1, 1, 4, 3, 6, 5, 7}
	for _, i := range order {
		out, _ := r.Push(pkt(start+uint16(i), fmt.Sprint(i), now))
		now += 100 * time.Millisecond
		for _, p := range out {
			got = append(got, p.Seq)
		}
	}
	want := []uint16{65530, 65531, 65532, 65533, 65534, 65535, 0}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if r.Duplicates != 1 {
		t.Fatalf("duplicates=%d", r.Duplicates)
	}
	if r.Pending() != 1 {
		t.Fatalf("pending=%d", r.Pending())
	}
	out, _ := r.Flush()
	if len(out) != 1 || out[0].Seq != 1 {
		t.Fatalf("flush=%+v", out)
	}
}

func TestJitterHoldAllowsReorderAfterSuccessor(t *testing.T) {
	r := NewReorderer()
	now := time.Duration(0)
	send := func(seq uint16) []Packet {
		out, _ := r.Push(pkt(seq, "p", now))
		now += 5 * time.Millisecond
		return out
	}
	if out := send(10); len(out) != 0 {
		t.Fatalf("packet released before jitter wait: %+v", out)
	}
	if out := send(12); len(out) != 0 {
		t.Fatalf("gap declared before wait: %+v", out)
	}
	if out := send(11); len(out) != 0 {
		t.Fatalf("packets released immediately after fill: %+v", out)
	}
	now += DefaultJitterWait
	out := r.Advance(now)
	want := []uint16{10, 11, 12}
	if len(out) != 3 {
		t.Fatalf("out=%+v", out)
	}
	for i, p := range out {
		if p.Seq != want[i] {
			t.Fatalf("seq[%d]=%d", i, p.Seq)
		}
	}
	if len(r.Declared) != 0 {
		t.Fatalf("reorder within window declared a gap: %+v", r.Declared)
	}
}

func TestJitterWaitAdaptsWithinBounds(t *testing.T) {
	r := NewReorderer()
	seen := map[time.Duration]bool{}
	for _, bw := range []int64{500_000, 4_000_000, 20_000_000, 4_000_000} {
		for i := 0; i < 20; i++ {
			r.SetBandwidth(bw)
		}
		w := r.Wait()
		if w < MinJitterWait || w > MaxJitterWait {
			t.Fatalf("wait %s outside bounds", w)
		}
		seen[w] = true
	}
	if len(seen) < 2 {
		t.Fatalf("jitter did not adapt: %v", seen)
	}
}

func TestPionWireRoundTrip(t *testing.T) {
	in := Packet{Seq: 65535, PayloadType: PayloadType, Marker: true, Timestamp: 90000,
		SSRC: SSRC, Payload: []byte("ts-payload")}
	wire, err := EncodeWire(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeWire(wire, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if out.Seq != in.Seq || out.Marker != in.Marker || out.Timestamp != in.Timestamp ||
		out.SSRC != in.SSRC || string(out.Payload) != string(in.Payload) || out.Arrival != 5*time.Millisecond {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if _, err := DecodeWire([]byte{0x80}, 0); err == nil {
		t.Fatal("short packet accepted")
	}
}

func TestReceiverAcceptsWirePackets(t *testing.T) {
	rc := NewReceiver()
	var got []Packet
	for i := 0; i < 4; i++ {
		wire, err := EncodeWire(Packet{Seq: uint16(500 + i), Payload: []byte{byte(i)}})
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := rc.PushWire(wire, time.Duration(i)*10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out...)
	}
	got = append(got, func() []Packet { o, _ := rc.Flush(); return o }()...)
	if len(got) != 4 {
		t.Fatalf("got %d packets", len(got))
	}
	for i := range got {
		if got[i].Seq != uint16(500+i) {
			t.Fatalf("order broken at %d: %d", i, got[i].Seq)
		}
	}
}

func TestSingleLossRecoveredByXOR(t *testing.T) {
	rc := NewReceiver()
	parity, _ := XORParity([][]byte{[]byte("alpha-01"), []byte("bravo-02")})
	rc.AddFEC(FECBlock{Base: 100, Count: 2, Lengths: []int{8, 8}, Parity: parity})
	parity2, _ := XORParity([][]byte{[]byte("charlie3"), []byte("echo-05")})
	rc.AddFEC(FECBlock{Base: 102, Count: 2, Lengths: []int{8, 8}, Parity: parity2})

	var all []Packet
	now := time.Duration(0)
	deliver := func(seq uint16, payload string, step time.Duration) {
		out, _ := rc.Push(pkt(seq, payload, now))
		now += step
		all = append(all, out...)
	}
	deliver(100, "alpha-01", 220*time.Millisecond)
	deliver(101, "bravo-02", 220*time.Millisecond)
	deliver(103, "echo-05", 220*time.Millisecond)  // gap deadline starts
	deliver(104, "foxtrot6", 220*time.Millisecond) // deadline expires
	tail, _ := rc.Flush()
	all = append(all, tail...)
	if st := rc.Stats(); st.Recovered != 1 || st.Unrecoverable != 0 {
		t.Fatalf("stats %+v", st)
	}
	if len(all) != 5 {
		t.Fatalf("emitted %d packets: %+v", len(all), all)
	}
	want := []uint16{100, 101, 102, 103, 104}
	for i, p := range all {
		if p.Seq != want[i] {
			t.Fatalf("seq[%d]=%d want %d", i, p.Seq, want[i])
		}
	}
	if string(all[2].Payload) != "charlie3" {
		t.Fatalf("repaired payload=%q", all[2].Payload)
	}
	all = all[:0]
	deliver(104, "foxtrot6", 220*time.Millisecond)
	if len(all) != 0 {
		t.Fatalf("duplicate emitted: %+v", all)
	}
}
