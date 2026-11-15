package tsdemux

import (
	"bytes"
	"io"
	"testing"
)

func mkPacket(pid uint16, cc uint8) []byte {
	p := make([]byte, 188)
	p[0] = 0x47
	p[1] = byte(pid >> 8)
	p[2] = byte(pid)
	p[3] = 0x10 | cc
	for i := 4; i < 188; i++ {
		p[i] = 0xAA
	}
	return p
}

func drain(t *testing.T, d *Demuxer) []*Packet {
	t.Helper()
	var out []*Packet
	for {
		p, err := d.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		cp := *p
		out = append(out, &cp)
	}
}

func TestParse188(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(mkPacket(100, 3))
	buf.Write(mkPacket(0x1FFF, 0))
	d := New(&buf)
	pkts := drain(t, d)
	if len(pkts) != 2 {
		t.Fatalf("got %d packets", len(pkts))
	}
	if pkts[0].PID != 100 || pkts[0].CC != 3 || !pkts[0].HasPayload {
		t.Fatalf("bad packet: %+v", pkts[0])
	}
	if d.Stats.PacketSize != 188 || d.Stats.Packets != 2 {
		t.Fatalf("bad stats: %+v", d.Stats)
	}
}

func TestParse192(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		buf.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF})
		buf.Write(mkPacket(uint16(50+i), uint8(i)))
	}
	d := New(&buf)
	pkts := drain(t, d)
	if len(pkts) != 3 || d.Stats.PacketSize != 192 {
		t.Fatalf("got %d packets size %d", len(pkts), d.Stats.PacketSize)
	}
	if pkts[2].PID != 52 {
		t.Fatalf("bad pid %d", pkts[2].PID)
	}
}

func TestResyncAfterGarbage(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(mkPacket(1, 0))
	buf.Write(mkPacket(1, 1))
	buf.Write(make([]byte, 5*188)) // garbage without sync bytes
	buf.Write(mkPacket(1, 2))
	buf.Write(mkPacket(1, 3))
	d := New(&buf)
	pkts := drain(t, d)
	if d.Stats.SyncLoss == 0 {
		t.Fatal("expected sync loss to be reported")
	}
	if len(pkts) != 4 || pkts[3].CC != 3 {
		t.Fatalf("got %d packets, last %+v", len(pkts), pkts[len(pkts)-1])
	}
}

func TestTailBytesIgnored(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(mkPacket(1, 0))
	buf.Write(mkPacket(1, 1))
	buf.Write([]byte{0x47, 0x00, 0x00}) // partial packet
	d := New(&buf)
	pkts := drain(t, d)
	if len(pkts) != 2 || d.Stats.TailBytes != 3 {
		t.Fatalf("got %d packets tail %d", len(pkts), d.Stats.TailBytes)
	}
}

func TestPCRAndAdaptation(t *testing.T) {
	p := mkPacket(0x100, 7)
	p[3] = 0x30 | 7 // adaptation + payload
	p[4] = 7
	p[5] = 0x90 // discontinuity + PCR flag
	base := int64(90000 * 12)
	p[6] = byte(base >> 25)
	p[7] = byte(base >> 17)
	p[8] = byte(base >> 9)
	p[9] = byte(base >> 1)
	p[10] = byte(base<<7) | 0x7E
	p[11] = 0
	d := New(bytes.NewReader(p))
	pkt, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !pkt.HasPCR || pkt.PCR90 != base {
		t.Fatalf("pcr got=%d want=%d", pkt.PCR90, base)
	}
	if !pkt.Discontinuity {
		t.Fatal("discontinuity flag not parsed")
	}
	if !pkt.HasPayload || len(pkt.Payload) != 188-12 {
		t.Fatalf("payload len %d", len(pkt.Payload))
	}
}
