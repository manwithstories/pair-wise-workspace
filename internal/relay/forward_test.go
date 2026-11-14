package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestForwarderRewritesIdentityAndPreservesTimestamps(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stats := NewStats()
	forwarder, err := NewUDPForwarder(listener.LocalAddr().String(), 0xCAFE, 500, stats)
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	received := make(chan *rtp.Packet, 2)
	go func() {
		buf := make([]byte, 1500)
		for i := 0; i < 2; i++ {
			_ = listener.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, _, err := listener.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt := &rtp.Packet{}
			if pkt.Unmarshal(buf[:n]) == nil {
				received <- pkt
			}
		}
	}()
	if err := forwarder.Forward(testPacket(10, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := forwarder.Forward(testPacket(11, 1180)); err != nil {
		t.Fatal(err)
	}
	first := <-received
	second := <-received
	if first.SSRC != 0xCAFE || second.SSRC != 0xCAFE {
		t.Fatalf("SSRC = %x/%x", first.SSRC, second.SSRC)
	}
	if first.SequenceNumber != 500 || second.SequenceNumber != 501 {
		t.Fatalf("sequence = %d/%d", first.SequenceNumber, second.SequenceNumber)
	}
	if first.Timestamp != 1000 || second.Timestamp != 1180 || second.Timestamp-first.Timestamp != 180 {
		t.Fatalf("timestamps = %d/%d", first.Timestamp, second.Timestamp)
	}
	metrics := stats.Snapshot()
	if metrics.Forwarded != 2 || metrics.ForwardBytes == 0 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

type flakyWriter struct {
	mu       sync.Mutex
	failures int
	writes   int
	closed   bool
}

func (w *flakyWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.failures > 0 {
		w.failures--
		return 0, errors.New("temporary sink failure")
	}
	return len(b), nil
}

func (w *flakyWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *flakyWriter) RemoteAddr() net.Addr             { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9} }
func (w *flakyWriter) Close() error                     { w.closed = true; return nil }

func TestForwarderRetriesOnceThenCountsError(t *testing.T) {
	stats := NewStats()
	writer := &flakyWriter{failures: 1}
	forwarder := newForwarderWithWriter(writer, 1, 1, stats)
	if err := forwarder.Forward(testPacket(1, 1)); err != nil {
		t.Fatalf("first retry should succeed: %v", err)
	}
	if writer.writes != 2 || stats.Snapshot().Forwarded != 1 {
		t.Fatalf("writes=%d metrics=%+v", writer.writes, stats.Snapshot())
	}
	writer.failures = 2
	if err := forwarder.Forward(testPacket(2, 2)); err == nil {
		t.Fatal("expected both write attempts to fail")
	}
	metrics := stats.Snapshot()
	if writer.writes != 4 || metrics.ForwardErrors != 1 || metrics.Forwarded != 1 {
		t.Fatalf("writes=%d metrics=%+v", writer.writes, metrics)
	}
}

func TestForwarderRejectsNonLoopbackSink(t *testing.T) {
	_, err := NewUDPForwarder("192.0.2.1:5004", 1, 1, NewStats())
	if !errors.Is(err, ErrNonLoopbackTarget) {
		t.Fatalf("error = %v", err)
	}
}

func TestSinkDrainsPackets(t *testing.T) {
	sink, err := ListenSink("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	runDone := make(chan error, 1)
	go func() { runDone <- sink.Run(done) }()
	if err := CheckSink(context.Background(), sink.Addr()); err != nil {
		t.Fatalf("sink readiness: %v", err)
	}
	if packets, _ := sink.Counts(); packets != 0 {
		t.Fatalf("probe was counted as media: %d", packets)
	}
	conn, err := net.Dial("udp", sink.Addr())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := fmt.Fprintf(conn, "packet-%d", i); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		packets, _ := sink.Counts()
		if packets == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink packets = %d, want 3", packets)
		}
		time.Sleep(time.Millisecond)
	}
	close(done)
	_ = conn.Close()
	_ = sink.Close()
	if err := <-runDone; err != nil {
		t.Fatalf("sink run: %v", err)
	}
}

func TestCheckSinkFailsWhenAbsent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := CheckSink(ctx, "127.0.0.1:64998"); err == nil {
		t.Fatal("expected absent sink readiness failure")
	}
}
