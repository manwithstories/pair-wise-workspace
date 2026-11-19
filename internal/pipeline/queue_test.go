package pipeline

import (
	"context"
	"testing"
	"time"

	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/stream"
)

func mkRawPacket(seq uint16, size int) *stream.Packet {
	return &stream.Packet{SSRC: 1, Sequence: seq, ExtSeq: uint64(seq), Raw: make([]byte, size)}
}

func TestConfigValidation(t *testing.T) {
	if merr := (Config{}).Validate(); merr == nil || merr.Kind != mediaerr.KindInvalidInput {
		t.Fatalf("zero config must error, got %v", merr)
	}
	good := Config{MaxBufferBytes: 100, MaxBufferPackets: 10, ReorderWindow: 8}
	if merr := good.Validate(); merr != nil {
		t.Fatalf("valid config rejected: %v", merr)
	}
}

func TestQueuePacketCapBackpressure(t *testing.T) {
	q := newBoundedQueue(1<<20, 2)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if merr := q.Send(ctx, mkRawPacket(uint16(i), 4)); merr != nil {
			t.Fatal(merr)
		}
	}
	if q.Len() != 2 {
		t.Fatalf("len=%d want 2", q.Len())
	}
	// A third send must block; prove it by canceling a short-lived context.
	ctx2, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	done := make(chan *mediaerr.Error, 1)
	go func() { done <- q.Send(ctx2, mkRawPacket(2, 4)) }()
	select {
	case merr := <-done:
		if merr == nil || merr.Kind != mediaerr.KindCanceled {
			t.Fatalf("full queue must block then cancel, got %v", merr)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("send did not block on full queue")
	}
	// Draining one packet unblocks a fresh sender.
	if _, ok, merr := q.Receive(ctx); merr != nil || !ok {
		t.Fatalf("receive failed: %v", merr)
	}
	if merr := q.Send(ctx, mkRawPacket(3, 4)); merr != nil {
		t.Fatalf("send after drain must succeed: %v", merr)
	}
}

func TestQueueByteBudget(t *testing.T) {
	q := newBoundedQueue(30, 100) // tight byte budget, roomy packet cap
	ctx := context.Background()
	if merr := q.Send(ctx, mkRawPacket(0, 20)); merr != nil {
		t.Fatal(merr)
	}
	// 20 + 20 > 30: the next send blocks until bytes are freed.
	ctx2, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	done := make(chan *mediaerr.Error, 1)
	go func() { done <- q.Send(ctx2, mkRawPacket(1, 20)) }()
	merr := <-done
	if merr == nil || merr.Kind != mediaerr.KindCanceled {
		t.Fatalf("byte budget must block, got %v", merr)
	}
	q.Receive(ctx) // free the 20 bytes
	if merr := q.Send(ctx, mkRawPacket(2, 20)); merr != nil {
		t.Fatalf("send within byte budget must pass: %v", merr)
	}
	if q.PeakBytes() < 20 {
		t.Fatalf("peak byte tracking wrong: %d", q.PeakBytes())
	}
}

func TestQueueDrainAndClose(t *testing.T) {
	q := newBoundedQueue(1<<20, 4)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if merr := q.Send(ctx, mkRawPacket(uint16(i), 2)); merr != nil {
			t.Fatal(merr)
		}
	}
	q.Close()
	got := 0
	for {
		p, ok, merr := q.Receive(ctx)
		if merr != nil {
			t.Fatal(merr)
		}
		if !ok {
			break
		}
		if p == nil {
			t.Fatal("nil packet before drain complete")
		}
		got++
	}
	if got != 3 {
		t.Fatalf("drained %d want 3", got)
	}
	if q.Bytes() != 0 {
		t.Fatalf("bytes must return to zero after drain: %d", q.Bytes())
	}
}

func TestQueueConcurrentRace(t *testing.T) {
	q := newBoundedQueue(40, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const n = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			if merr := q.Send(ctx, mkRawPacket(uint16(i), 10)); merr != nil {
				t.Errorf("send %d: %v", i, merr)
				return
			}
		}
		q.Close()
	}()
	got := 0
	for {
		p, ok, merr := q.Receive(ctx)
		if merr != nil {
			t.Fatalf("receive: %v", merr)
		}
		if !ok {
			break
		}
		if p == nil {
			t.Fatal("nil packet")
		}
		got++
	}
	if got != n {
		t.Fatalf("consumed %d want %d", got, n)
	}
	<-done
}

func TestQueueCancelReleasesWaiters(t *testing.T) {
	q := newBoundedQueue(1<<20, 1)
	ctx, cancel := context.WithCancel(context.Background())
	if merr := q.Send(ctx, mkRawPacket(0, 1)); merr != nil {
		t.Fatal(merr)
	}
	done := make(chan *mediaerr.Error, 1)
	go func() { done <- q.Send(ctx, mkRawPacket(1, 1)) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case merr := <-done:
		if merr == nil || merr.Kind != mediaerr.KindCanceled {
			t.Fatalf("cancel must release blocked sender: %v", merr)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("canceled sender still blocked")
	}
}
