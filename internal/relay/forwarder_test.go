package relay

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func newTestForwarder(t *testing.T, faults Faults, seed int64, rewrite *uint32) (*Forwarder, *net.UDPConn) {
	t.Helper()
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	forwarder, err := New(Config{
		Target: listener.LocalAddr().(*net.UDPAddr), Faults: faults, Seed: seed,
		PacketLog: filepath.Join(t.TempDir(), "packets.jsonl"), RewriteSSRC: rewrite,
		IdleTimeout: time.Second,
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = forwarder.Close()
		_ = listener.Close()
	})
	return forwarder, listener
}

func testPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: uint32(seq), SSRC: 42},
		Payload: []byte{1, 2, 3, 4},
	}
}

func runFaultProfile(t *testing.T, faults Faults, seed int64) Counts {
	t.Helper()
	forwarder, _ := newTestForwarder(t, faults, seed, nil)
	start := time.Now()
	for i := 0; i < 100; i++ {
		if err := forwarder.Handle(testPacket(uint16(i)), start.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	forwarder.Flush()
	return forwarder.Counts()
}

func TestFaultInjectionIsSeedReproducible(t *testing.T) {
	faults := Faults{Drop: 0.2, Duplicate: 0.2, Reorder: 0.2}
	first := runFaultProfile(t, faults, 7)
	second := runFaultProfile(t, faults, 7)
	if first != second {
		t.Fatalf("same seed differs: %+v != %+v", first, second)
	}
	if first.Received != 100 || first.Dropped == 0 || first.Duplicated == 0 || first.Reordered == 0 {
		t.Fatalf("unexpected injected counts: %+v", first)
	}
	wantForwarded := first.Received - first.Dropped + first.Duplicated
	if first.Forwarded != wantForwarded || first.WriteErrors != 0 {
		t.Fatalf("forwarded = %d, want %d (%+v)", first.Forwarded, wantForwarded, first)
	}
}

func TestForwarderWritesOneRecordPerInputPacket(t *testing.T) {
	forwarder, _ := newTestForwarder(t, Faults{Drop: 0.1, Duplicate: 0.1, Reorder: 0.1}, 9, nil)
	for i := 0; i < 30; i++ {
		if err := forwarder.Handle(testPacket(uint16(65530+i)), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	forwarder.Flush()
	file, err := os.Open(forwarder.cfg.PacketLog)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	lines := 0
	for scanner.Scan() {
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != 30 {
		t.Fatalf("packet log lines = %d, want 30", lines)
	}
}

func TestForwarderRewritesSSRCAndWrapsSequence(t *testing.T) {
	rewritten := uint32(0xCAFE)
	forwarder, listener := newTestForwarder(t, Faults{}, 1, &rewritten)
	sequences := []uint16{65534, 65535, 0, 1}
	for _, sequence := range sequences {
		if err := forwarder.Handle(testPacket(sequence), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	forwarder.Flush()
	buffer := make([]byte, 2048)
	_ = listener.SetReadDeadline(time.Now().Add(time.Second))
	for _, want := range sequences {
		n, _, err := listener.ReadFromUDP(buffer)
		if err != nil {
			t.Fatal(err)
		}
		var packet rtp.Packet
		if err := packet.Unmarshal(buffer[:n]); err != nil {
			t.Fatal(err)
		}
		if packet.SequenceNumber != want || packet.SSRC != rewritten {
			t.Fatalf("seq/ssrc = %d/%d, want %d/%d", packet.SequenceNumber, packet.SSRC, want, rewritten)
		}
	}
}

func TestForwarderConcurrentHandleIsSafe(t *testing.T) {
	rewritten := uint32(0xBEEF)
	packets := 64
	forwarder, _ := newTestForwarder(t, Faults{}, 1, &rewritten)
	var wg sync.WaitGroup
	for i := 0; i < packets; i++ {
		wg.Add(1)
		go func(sequence uint16) {
			defer wg.Done()
			if err := forwarder.Handle(testPacket(sequence), time.Now()); err != nil {
				t.Errorf("handle packet: %v", err)
			}
		}(uint16(i))
	}
	wg.Wait()
	counts := forwarder.Counts()
	if counts.Received != uint64(packets) || counts.Forwarded != uint64(packets) {
		t.Fatalf("unexpected counts: %+v", counts)
	}
}
