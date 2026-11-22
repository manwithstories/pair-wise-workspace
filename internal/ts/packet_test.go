package ts

import (
	"testing"

	"mediatool/internal/clock"
)

// buildPacket 构造一个带负载的测试包。
func buildPacket(pid uint16, cc uint8, payloadLen int) []byte {
	pkt := make([]byte, PacketSize)
	pkt[0] = SyncByte
	pkt[1] = byte(pid >> 8)
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | cc
	for i := 4; i < 4+payloadLen && i < PacketSize; i++ {
		pkt[i] = byte(i)
	}
	return pkt
}

func TestParseRoundTrip(t *testing.T) {
	var data []byte
	for i := 0; i < 5; i++ {
		data = append(data, buildPacket(0x101, uint8(i), 100)...)
	}
	res := Parse(data)
	if len(res.Faults) != 0 {
		t.Fatalf("unexpected faults: %v", res.Faults)
	}
	if len(res.Packets) != 5 {
		t.Fatalf("packets = %d, want 5", len(res.Packets))
	}
	for i, p := range res.Packets {
		if got := p.Marshal(); string(got) != string(data[i*PacketSize:(i+1)*PacketSize]) {
			t.Fatalf("packet %d roundtrip mismatch", i)
		}
		if p.Offset != int64(i*PacketSize) {
			t.Fatalf("packet %d offset = %d", i, p.Offset)
		}
	}
}

func TestParseTruncatedTail(t *testing.T) {
	data := append(buildPacket(0x101, 0, 50), make([]byte, 37)...)
	res := Parse(data)
	if len(res.Packets) != 1 {
		t.Fatalf("packets = %d, want 1", len(res.Packets))
	}
	var found *clock.Fault
	for i := range res.Faults {
		if res.Faults[i].Kind == clock.FaultTruncatedPacket {
			found = &res.Faults[i]
		}
	}
	if found == nil {
		t.Fatal("missing truncated-packet fault")
	}
	if found.Offset != PacketSize {
		t.Fatalf("truncated offset = %d, want %d", found.Offset, PacketSize)
	}
}

func TestParseSyncLossReportsFirstOffset(t *testing.T) {
	good := buildPacket(0x101, 0, 10)
	bad := buildPacket(0x101, 1, 10)
	bad[0] = 0x00
	data := append(append(good, bad...), buildPacket(0x101, 2, 10)...)
	res := Parse(data)
	if res.Faults.FirstOffset() != PacketSize {
		t.Fatalf("first fault offset = %d, want %d", res.Faults.FirstOffset(), PacketSize)
	}
}

func TestCCClassification(t *testing.T) {
	mk := func(cc uint8) Packet {
		return Packet{PID: 0x101, HasPayload: true, CC: cc}
	}
	tr := NewCCTracker()
	if f := tr.Track(mk(3)); len(f) != 0 {
		t.Fatalf("first packet: %v", f)
	}
	if f := tr.Track(mk(4)); len(f) != 0 {
		t.Fatalf("in-order: %v", f)
	}
	f := tr.Track(mk(4)) // 重复
	if len(f) != 1 || f[0].Kind != clock.FaultCCDuplicate || !f[0].Recovered {
		t.Fatalf("duplicate: %+v", f)
	}
	f = tr.Track(mk(7)) // 缺口 5,6 丢失
	if len(f) != 1 || f[0].Kind != clock.FaultCCGap || !f[0].Recovered {
		t.Fatalf("gap: %+v", f)
	}
	f = tr.Track(mk(2)) // 倒退，不可恢复
	if len(f) != 1 || f[0].Kind != clock.FaultCCConflict || f[0].Recovered {
		t.Fatalf("conflict: %+v", f)
	}
}

func TestNullPacketsIgnored(t *testing.T) {
	tr := NewCCTracker()
	if f := tr.Track(Packet{PID: NullPID, HasPayload: true, CC: 9}); len(f) != 0 {
		t.Fatalf("null packet should be ignored: %v", f)
	}
}
