package rtsp

import (
	"net"
	"testing"
	"time"
)

func TestUDPTransportSmoke(t *testing.T) {
	tr, ports, err := NewUDPTransport(0, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:49152")
	_ = addr
	target := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: ports[0]}
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0x80, 0x60, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	pkt, err := tr.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if len(pkt) != 15 {
		t.Fatalf("len=%d want 15", len(pkt))
	}
}
