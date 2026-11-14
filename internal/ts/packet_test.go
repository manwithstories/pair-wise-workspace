package ts

import (
	"bytes"
	"io"
	"testing"
)

func serializePacket(t *testing.T, pid uint16, cc uint8, pusi bool, payload []byte, adapt *Adaptation) []byte {
	t.Helper()
	var b bytes.Buffer
	w := NewWriter(&b)
	if err := w.Write(pid, cc, pusi, payload, adapt); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), b.Bytes()...)
}

func TestReaderResync188AndStats(t *testing.T) {
	p1 := serializePacket(t, 0x101, 0, true, []byte{1, 2, 3}, nil)
	p2 := serializePacket(t, 0x102, 1, false, []byte{4, 5}, nil)
	stream := append([]byte{0xde, 0xad}, p1...)
	stream = append(stream, 0xaa, 0xbb, 0xcc)
	stream = append(stream, p2...)
	rd, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	a, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if a.Header.PID != 0x101 || !a.Header.PayloadUnitStart || len(a.Payload) < 3 || a.Payload[0] != 1 {
		t.Fatalf("unexpected first packet: %+v", a)
	}
	b, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.Header.PID != 0x102 || b.Payload[0] != 4 {
		t.Fatalf("unexpected second packet: %+v", b)
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	st := rd.Stats()
	if st.PacketSize != PacketSize188 || st.PacketCount != 2 || st.SyncLosses != 2 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if st.GarbageBytes != 5 {
		t.Fatalf("garbage bytes = %d, want 5", st.GarbageBytes)
	}
}

func TestReader192BytePrefix(t *testing.T) {
	var stream []byte
	for i := 0; i < 3; i++ {
		p := serializePacket(t, uint16(0x200+i), uint8(i), i == 0, []byte{byte(i), 0x55}, nil)
		stream = append(stream, 0x11, 0x22, 0x33, 0x44)
		stream = append(stream, p...)
	}
	rd, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p, err := rd.Next()
		if err != nil {
			t.Fatal(err)
		}
		if p.Size != PacketSize192 || p.Header.PID != uint16(0x200+i) || p.Header.ContinuityCounter != uint8(i) {
			t.Fatalf("unexpected packet %d: %+v", i, p)
		}
	}
	if rd.Stats().PacketSize != PacketSize192 || rd.Stats().PacketCount != 3 {
		t.Fatalf("unexpected stats: %+v", rd.Stats())
	}
}

func TestReaderParsesPCR(t *testing.T) {
	p := serializePacket(t, 0x101, 7, true, []byte{9, 8, 7}, &Adaptation{HasPCR: true, PCRBase: 123456, PCRExtension: 42})
	rd, err := NewReader(bytes.NewReader(p))
	if err != nil {
		t.Fatal(err)
	}
	got, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Header.AdaptationField || !got.Adapt.HasPCR || got.Adapt.PCRBase != 123456 || got.Adapt.PCRExtension != 42 {
		t.Fatalf("unexpected PCR packet: %+v", got)
	}
}
