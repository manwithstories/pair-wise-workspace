package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestParsePacketAndTruncation(t *testing.T) {
	raw, err := BuildPacket(PacketOptions{PID: 0x100, PUSI: true, CC: 3, Payload: []byte("payload")})
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePacket(raw, 188)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != 0x100 || !p.PUSI || p.CC != 3 || p.Offset != 188 {
		t.Fatalf("bad header: %+v", p)
	}
	if !bytes.HasPrefix(p.Payload, []byte("payload")) {
		t.Fatalf("payload=%x", p.Payload[:16])
	}

	r := NewReader(bytes.NewReader(append(raw[:100], 0)))
	_, err = r.Read()
	var te *TruncatedError
	if !errors.As(err, &te) || te.Got != 101 {
		t.Fatalf("err=%v, want TruncatedError got=101", err)
	}
}

func TestCCGapDetectedOnce(t *testing.T) {
	var stream bytes.Buffer
	for _, cc := range []uint8{0, 1, 4, 5, 5, 6} {
		raw, err := BuildPacket(PacketOptions{PID: 0x101, CC: cc, Payload: []byte{0x01}})
		if err != nil {
			t.Fatal(err)
		}
		stream.Write(raw)
	}
	r := NewReader(&stream).WithCCCheck()
	for {
		_, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	gaps := r.Gaps()
	if len(gaps) != 1 {
		t.Fatalf("gaps=%+v, want one", gaps)
	}
	if gaps[0].Expected != 2 || gaps[0].Got != 4 || gaps[0].Missing != 2 {
		t.Fatalf("bad gap: %+v", gaps[0])
	}
}

func TestQueueBackpressureAndCancel(t *testing.T) {
	q := NewQueue(1, time.Hour)
	if err := q.Push(context.Background(), Packet{PID: 1}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, Packet{PID: 2}) }()

	select {
	case <-done:
		t.Fatal("second push should block on full queue")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Push err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Push did not exit")
	}

	popCtx, popCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer popCancel()
	_, err := q.Pop(popCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delayed Pop err=%v", err)
	}
}
