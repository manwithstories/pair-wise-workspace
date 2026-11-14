package tspipe

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func testPacket(pid uint16, cc uint8, pusi bool, payload []byte) []byte {
	b := bytes.Repeat([]byte{0xff}, PacketSize)
	b[0] = 0x47
	b[1], b[2], b[3] = 0, 0, 0
	if pusi {
		b[1] = 0x40
	}
	b[1] |= byte(pid >> 8)
	b[2] = byte(pid)
	b[3] = 0x10 | cc
	copy(b[4:], payload)
	return b
}

func TestReaderResyncAndPayload(t *testing.T) {
	stream := append([]byte{0x00, 0x11, 0x22}, testPacket(0x100, 7, true, []byte("hello"))...)
	stream = append(stream, testPacket(0x101, 3, false, []byte("world"))...)
	r := NewReader(bytes.NewReader(stream))
	first, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Offset != 3 || first.PID != 0x100 || first.Continuity != 7 || !first.PUSI {
		t.Fatalf("unexpected first packet: %+v", first)
	}
	if !bytes.HasPrefix(first.Payload, []byte("hello")) {
		t.Fatalf("payload = %q", first.Payload[:5])
	}
	second, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if second.Offset != 191 || second.PID != 0x101 || second.PUSI {
		t.Fatalf("unexpected second packet: %+v", second)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
	if r.SyncErrors() != 1 || r.LostBytes() != 3 {
		t.Fatalf("sync stats = %d, lost = %d", r.SyncErrors(), r.LostBytes())
	}
}

func TestReaderRejects192ByteEncapsulation(t *testing.T) {
	var stream []byte
	for i := 0; i < 5; i++ {
		stream = append(stream, 1, 2, 3, 4)
		stream = append(stream, testPacket(0x20, uint8(i), false, nil)...)
	}
	_, err := NewReader(bytes.NewReader(stream)).Next()
	if !errors.Is(err, ErrUnsupported192) {
		t.Fatalf("expected ErrUnsupported192, got %v", err)
	}
}

func TestReaderParsesAdaptationOnlyPacket(t *testing.T) {
	p := bytes.Repeat([]byte{0xff}, PacketSize)
	p[0] = 0x47
	p[1], p[2] = 0x01, 0x00
	p[3] = 0x20 | 9
	p[4] = 183
	p[5] = 0x80
	r := NewReader(bytes.NewReader(p))
	packet, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if packet.HasPayload || !packet.HasAdaptation || !packet.Discontinuity {
		t.Fatalf("unexpected adaptation flags: %+v", packet)
	}
}

func TestReaderNoSync(t *testing.T) {
	_, err := NewReader(bytes.NewReader(bytes.Repeat([]byte{0xaa}, 1000))).Next()
	if !errors.Is(err, ErrNoSync) {
		t.Fatalf("expected ErrNoSync, got %v", err)
	}
}
