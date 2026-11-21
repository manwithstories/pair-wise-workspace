package transport

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16, ts uint32) []byte {
	p := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 1}}
	raw, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}

func seqs(pkts []*rtp.Packet) []uint16 {
	out := make([]uint16, len(pkts))
	for i, p := range pkts {
		out[i] = p.SequenceNumber
	}
	return out
}

func TestInOrder(t *testing.T) {
	r := NewReorderer(32)
	var got []uint16
	for s := uint16(100); s < 105; s++ {
		got = append(got, seqs(r.Push(pkt(s, uint32(s*960))))...)
	}
	if len(got) != 5 || got[0] != 100 || got[4] != 104 {
		t.Fatalf("got %v", got)
	}
	st := r.Stats()
	if st.Expected != 5 || st.Received != 5 || st.Recovered != 0 || st.Lost != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestReorderAndDuplicate(t *testing.T) {
	r := NewReorderer(32)
	var got []uint16
	push := func(s uint16) { got = append(got, seqs(r.Push(pkt(s, 0)))...) }
	push(10)
	push(12) // 缓冲
	push(12) // 重复
	push(11) // 触发 11,12 交付
	push(10) // 迟到重复
	if len(got) != 3 || got[0] != 10 || got[1] != 11 || got[2] != 12 {
		t.Fatalf("got %v", got)
	}
	st := r.Stats()
	if st.Duplicates != 2 {
		t.Fatalf("duplicates: %+v", st)
	}
	if st.Recovered != 1 { // 仅 12 从缓冲交付
		t.Fatalf("recovered: %+v", st)
	}
	if st.Received != 3 || st.Expected != 3 {
		t.Fatalf("counts: %+v", st)
	}
}

func TestLossBeyondWindow(t *testing.T) {
	r := NewReorderer(4)
	var got []uint16
	for _, s := range []uint16{0, 1, 2, 10} { // 3..9 超窗丢失
		got = append(got, seqs(r.Push(pkt(s, 0)))...)
	}
	got = append(got, seqs(r.Flush())...)
	st := r.Stats()
	if st.Lost != 7 {
		t.Fatalf("lost: %+v", st)
	}
	if len(r.Faults) == 0 || r.Faults[0].Recoverable {
		t.Fatal("out-of-window loss must be recorded unrecoverable")
	}
	if got[len(got)-1] != 10 {
		t.Fatalf("got %v", got)
	}
}

func TestWindowLimit(t *testing.T) {
	if NewReorderer(0) != nil || NewReorderer(MaxWindow+1) != nil {
		t.Fatal("window bounds")
	}
}

func TestPacketTime(t *testing.T) {
	tm := PacketTime(48000, 48000)
	if tm.Millis() != 1000 {
		t.Fatalf("time: %v", tm)
	}
}
