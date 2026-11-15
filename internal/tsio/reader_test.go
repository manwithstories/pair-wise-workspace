package tsio

import (
	"bytes"
	"io"
	"testing"
)

func testPacket(pid uint16, cc, marker byte) []byte {
	p := make([]byte, PacketSize)
	p[0] = SyncByte
	p[1] = byte(pid >> 8)
	p[2] = byte(pid)
	p[3] = 0x10 | (cc & 0x0f)
	for i := 4; i < len(p); i++ {
		p[i] = marker
	}
	return p
}

func TestReaderResyncZeroFillAndTail(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(testPacket(0x100, 0, 0x11))
	stream.Write(bytes.Repeat([]byte{0xaa}, 37))
	stream.Write(testPacket(0x101, 0, 0x22))
	stream.Write(make([]byte, PacketSize))
	stream.Write(testPacket(0x102, 0, 0x33))
	stream.Write(bytes.Repeat([]byte{SyncByte}, 25))

	r := New(&stream)
	buf := make([]byte, PacketSize)
	var got int
	for {
		n, err := r.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if n != PacketSize {
			t.Fatalf("read %d bytes", n)
		}
		got++
	}
	if got < 2 {
		t.Fatalf("recovered %d packets, want at least 2", got)
	}
	c := r.Counters()
	if c.SyncLossBytes == 0 || c.SyncLossEvents == 0 {
		t.Fatalf("sync loss not counted: %+v", c)
	}
	if c.ZeroFillSegments == 0 || c.ZeroFillBytes < PacketSize {
		t.Fatalf("zero fill not counted: %+v", c)
	}
	if c.TrailingBytes == 0 {
		t.Fatalf("sync-byte tail not counted: %+v", c)
	}
}

func TestReaderAllGarbageTerminates(t *testing.T) {
	r := New(bytes.NewReader(bytes.Repeat([]byte{0x7f}, 4096)))
	buf := make([]byte, PacketSize)
	if _, err := r.Read(buf); err != io.EOF {
		t.Fatalf("got %v, want EOF", err)
	}
	if r.Counters().SyncLossBytes != 4096 {
		t.Fatalf("sync loss = %d", r.Counters().SyncLossBytes)
	}
}
