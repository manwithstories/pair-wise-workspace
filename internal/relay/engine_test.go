package relay

import (
	"errors"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestSequenceStateWrapsPerEgress(t *testing.T) {
	first := sequenceState{next: 65534}
	second := sequenceState{next: 100}
	var got []uint16
	for i := 0; i < 4; i++ {
		packet := &rtp.Packet{}
		first.apply(packet)
		got = append(got, packet.SequenceNumber)
		packet = &rtp.Packet{}
		second.apply(packet)
		if packet.SequenceNumber != uint16(100+i) {
			t.Fatalf("egress sequence state is not independent: %d", packet.SequenceNumber)
		}
	}
	want := []uint16{65534, 65535, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wrap mismatch: got %v want %v", got, want)
		}
	}
}

type flakyWriter struct {
	failures int
	calls    int
}

func (w *flakyWriter) WriteRTP(*rtp.Packet) error {
	w.calls++
	if w.calls <= w.failures {
		return errors.New("write failed")
	}
	return nil
}

func TestWriteRetryStopsAfterThreeRetries(t *testing.T) {
	writer := &flakyWriter{failures: 10}
	err := writeWithRetry(time.Millisecond, writer, &rtp.Packet{})
	if err == nil || writer.calls != 4 {
		t.Fatalf("expected four attempts and failure, calls=%d err=%v", writer.calls, err)
	}
}

func TestWriteRetryRecovers(t *testing.T) {
	writer := &flakyWriter{failures: 2}
	if err := writeWithRetry(time.Millisecond, writer, &rtp.Packet{}); err != nil {
		t.Fatal(err)
	}
	if writer.calls != 3 {
		t.Fatalf("expected three attempts, got %d", writer.calls)
	}
}

func TestEgressAddRemoveKeepsIngress(t *testing.T) {
	engine, err := NewEngine(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	publisher, err := engine.NewPublisher("room", "p-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.NewSubscriber("room", "s-2"); err != nil {
		t.Fatal(err)
	}
	engine.RemovePeer("room", "s-2")
	if _, err := engine.NewSubscriber("room", "s-3"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.NewPublisher("room", "p-4"); err == nil {
		t.Fatal("ingress was unexpectedly replaced")
	}
	if publisher == nil {
		t.Fatal("publisher disappeared")
	}
}

func TestEgressCapacity(t *testing.T) {
	engine, err := NewEngine(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.NewPublisher("room", "p-1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxEgress; i++ {
		if _, err := engine.NewSubscriber("room", "s-"+string(rune('a'+i))); err != nil {
			t.Fatalf("subscriber %d: %v", i, err)
		}
	}
	if _, err := engine.NewSubscriber("room", "overflow"); err == nil {
		t.Fatal("expected capacity error")
	}
}
