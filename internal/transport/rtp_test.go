package transport

import (
	"bytes"
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func TestReordersDeduplicatesAndReassemblesAU(t *testing.T) {
	au := bytes.Repeat([]byte{0x5a}, 2500)
	pkts := PacketizeAU(9000, au, 1000, 100, 7)
	if len(pkts) != 3 || !pkts[2].Marker {
		t.Fatalf("packetization wrong: %d packets marker=%v", len(pkts), pkts[len(pkts)-1].Marker)
	}
	r := NewReceiver(Config{})
	order := []int{2, 0, 0, 1}
	seen := map[int]bool{}
	for _, idx := range order {
		dup, err := r.Push(&pkts[idx], 1_000)
		if err != nil {
			t.Fatal(err)
		}
		if seen[idx] {
			if !dup {
				t.Fatalf("packet %d should be duplicate", idx)
			}
		} else if dup {
			t.Fatalf("packet %d unexpectedly duplicate", idx)
		}
		seen[idx] = true
	}
	if r.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", r.Duplicates)
	}
	got := r.Poll(2_000)
	if len(got) != 1 {
		t.Fatalf("Poll returned %d AUs: %+v", len(got), got)
	}
	if !got[0].Complete || !bytes.Equal(got[0].Data, au) {
		t.Fatalf("reassembly wrong complete=%v len=%d", got[0].Complete, len(got[0].Data))
	}
}

func TestWindowGapMarksAUCorrupt(t *testing.T) {
	r := NewReceiver(Config{})
	mk := func(seq uint16, marker bool, payload byte) rtp.Packet {
		return rtp.Packet{Header: rtp.Header{
			Version: 2, SequenceNumber: seq, Timestamp: 42, Marker: marker,
		}, Payload: []byte{payload}}
	}
	p20 := mk(20, false, 'a')
	if _, err := r.Push(&p20, 1000); err != nil {
		t.Fatal(err)
	}
	p22 := mk(22, true, 'c')
	if _, err := r.Push(&p22, 1000); err != nil {
		t.Fatal(err)
	}
	if got := r.Poll(1000 + MaxWaitNS - 1); len(got) != 0 {
		t.Fatalf("AU should wait for missing packet, got %+v", got)
	}
	got := r.Poll(1000 + MaxWaitNS)
	if len(got) != 1 || got[0].Complete {
		t.Fatalf("expected one incomplete AU: %+v", got)
	}
	if len(got[0].Missing) != 1 || got[0].Missing[0] != 21 {
		t.Fatalf("missing=%v want [21]", got[0].Missing)
	}
	if string(got[0].Data) != "ac" {
		t.Fatalf("available payload=%q want ac", got[0].Data)
	}
}

func TestTimeoutWithoutMarkerRecordsTailLoss(t *testing.T) {
	r := NewReceiver(Config{})
	p0 := rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 30, Timestamp: 99}, Payload: []byte("x")}
	p2 := rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 32, Timestamp: 99}, Payload: []byte("z")}
	if _, err := r.Push(&p0, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Push(&p2, 500); err != nil {
		t.Fatal(err)
	}
	got := r.Poll(500 + MaxWaitNS)
	if len(got) != 1 {
		t.Fatalf("got %d AUs", len(got))
	}
	au := got[0]
	if au.Complete || au.MarkerReceived {
		t.Fatalf("AU should be incomplete without marker: %+v", au)
	}
	if len(au.Missing) != 1 || au.Missing[0] != 31 {
		t.Fatalf("missing=%v want [31]", au.Missing)
	}
}

func TestCacheIsHardBoundedAndSignalsBackpressure(t *testing.T) {
	r := NewReceiver(Config{})
	for i := 0; i < MaxPackets; i++ {
		p := rtp.Packet{Header: rtp.Header{
			Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i + 1),
		}, Payload: []byte{byte(i)}}
		if _, err := r.Push(&p, 100); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if !r.Backpressure() {
		t.Fatal("expected backpressure at packet limit")
	}
	extra := rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 512, Timestamp: 777}}
	_, err := r.Push(&extra, 100)
	if !errors.Is(err, ErrBackpressure) {
		t.Fatalf("err=%v want ErrBackpressure", err)
	}
	if r.BufferedPackets() != MaxPackets {
		t.Fatalf("buffered=%d want %d", r.BufferedPackets(), MaxPackets)
	}
	aus := r.Poll(100 + MaxWaitNS)
	if len(aus) != MaxPackets {
		t.Fatalf("expired AUs=%d want %d", len(aus), MaxPackets)
	}
	if r.Backpressure() {
		t.Fatal("backpressure should clear after Poll")
	}
}

func TestSequenceWrapReassembles(t *testing.T) {
	au := []byte("wrap-around-access-unit")
	pkts := PacketizeAU(7, au, 8, 65534, 1)
	r := NewReceiver(Config{})
	for i := len(pkts) - 1; i >= 0; i-- {
		if _, err := r.Push(&pkts[i], 0); err != nil {
			t.Fatal(err)
		}
	}
	got := r.Poll(1)
	if len(got) != 1 || !got[0].Complete || string(got[0].Data) != string(au) {
		t.Fatalf("wrap reassembly failed: %+v", got)
	}
}

func TestReceiveDatagramUsesPionUnmarshal(t *testing.T) {
	src := &rtp.Packet{
		Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: 9, Timestamp: 3, SSRC: 4},
		Payload: []byte("ok"),
	}
	raw, err := src.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	r := NewReceiver(Config{})
	dup, err := r.ReceiveDatagram(raw, 0)
	if err != nil || dup {
		t.Fatalf("ReceiveDatagram dup=%v err=%v", dup, err)
	}
	got := r.Poll(0)
	if len(got) != 1 || string(got[0].Data) != "ok" {
		t.Fatalf("bad datagram AU: %+v", got)
	}
}
