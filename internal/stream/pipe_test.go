package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mediadeliver/internal/model"
)

func mkPkt(seq int64, size int) *model.Packet {
	return &model.Packet{Seq: seq, Payload: make([]byte, size)}
}

func TestPushPopDrainInOrder(t *testing.T) {
	q := NewQueue(4, 1024)
	ctx := context.Background()
	for i := int64(0); i < 4; i++ {
		if err := q.Push(ctx, mkPkt(i, 10)); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	q.Close()
	for i := int64(0); i < 4; i++ {
		p, err := q.Pop(ctx)
		if err != nil || p == nil || p.Seq != i {
			t.Fatalf("pop %d: p=%v err=%v", i, p, err)
		}
	}
	p, err := q.Pop(ctx) // after drain -> EOS
	if err != nil || p != nil {
		t.Fatalf("expected EOS, got p=%v err=%v", p, err)
	}
}

func TestBackpressureBlocksSlowConsumer(t *testing.T) {
	q := NewQueue(2, 4096)
	ctx := context.Background()
	if err := q.Push(ctx, mkPkt(0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(ctx, mkPkt(1, 1)); err != nil {
		t.Fatal(err)
	}
	// Third push must block until the consumer drains one.
	pushed := make(chan error, 1)
	go func() { pushed <- q.Push(ctx, mkPkt(2, 1)) }()
	select {
	case err := <-pushed:
		t.Fatalf("push should have blocked, returned %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := q.Pop(ctx); err != nil { // make room
		t.Fatal(err)
	}
	select {
	case err := <-pushed:
		if err != nil {
			t.Fatalf("blocked push failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer never unblocked after drain")
	}
	if q.Stats().BackpressureEvents < 1 {
		t.Fatalf("expected backpressure event, stats=%+v", q.Stats())
	}
}

func TestOversizePacketRejected(t *testing.T) {
	q := NewQueue(4, 8)
	err := q.Push(context.Background(), mkPkt(0, 9))
	if model.KindOf(err) != model.KindLimitExceeded {
		t.Fatalf("want KindLimitExceeded, got %v", err)
	}
	if model.PositionOf(err) != 0 {
		t.Fatalf("want position 0, got %d", model.PositionOf(err))
	}
	if q.Stats().RejectedOversize != 1 {
		t.Fatalf("reject counter: %d", q.Stats().RejectedOversize)
	}
}

func TestCancellationPropagates(t *testing.T) {
	q := NewQueue(1, 4096)
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Push(ctx, mkPkt(0, 1)); err != nil {
		t.Fatal(err)
	}
	// Full queue: a blocked push must return KindCancelled on cancel.
	blockedPush := make(chan error, 1)
	go func() { blockedPush <- q.Push(ctx, mkPkt(1, 1)) }()
	time.Sleep(20 * time.Millisecond)

	// Empty-ish consumer on a second queue: blocked pop also cancels.
	q2 := NewQueue(1, 4096)
	blockedPop := make(chan error, 1)
	go func() {
		_, err := q2.Pop(ctx)
		blockedPop <- err
	}()
	time.Sleep(20 * time.Millisecond)

	cancel()
	for name, ch := range map[string]chan error{"push": blockedPush, "pop": blockedPop} {
		select {
		case err := <-ch:
			if model.KindOf(err) != model.KindCancelled {
				t.Fatalf("%s: want KindCancelled, got %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not react to cancellation", name)
		}
	}
}

func TestFailurePropagatesToAllStages(t *testing.T) {
	ctx := context.Background()
	terminal := model.New(model.KindIO, "remux", "write", "disk full")

	// fullQueue: a push is blocked waiting for room.
	fullQueue := NewQueue(1, 4096)
	if err := fullQueue.Push(ctx, mkPkt(0, 1)); err != nil {
		t.Fatal(err)
	}
	// emptyQueue: a pop is blocked waiting for data.
	emptyQueue := NewQueue(1, 4096)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = fullQueue.Push(ctx, mkPkt(1, 1)) }()
	go func() { defer wg.Done(); _, errs[1] = emptyQueue.Pop(ctx) }()

	time.Sleep(30 * time.Millisecond) // let both stages block
	fullQueue.FailWith(terminal)
	emptyQueue.FailWith(terminal)
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, terminal) {
			t.Fatalf("stage %d: want terminal error, got %v", i, err)
		}
	}
	// First error is preserved, later FailWith must not overwrite.
	fullQueue.FailWith(errors.New("later"))
	if !errors.Is(fullQueue.failed, terminal) {
		t.Fatal("first terminal error was overwritten")
	}
}

func TestPeakBufferBoundedByCapacity(t *testing.T) {
	const capN, pktBytes = 4, 100
	q := NewQueue(capN, 10_000)
	ctx := context.Background()
	for i := 0; i < capN; i++ {
		if err := q.Push(ctx, mkPkt(int64(i), pktBytes)); err != nil {
			t.Fatal(err)
		}
	}
	st := q.Stats()
	if st.PeakBufferedPackets > capN {
		t.Fatalf("peak packets %d > capacity %d", st.PeakBufferedPackets, capN)
	}
	if st.PeakBufferBytes > capN*pktBytes {
		t.Fatalf("peak bytes %d exceeds bound %d", st.PeakBufferBytes, capN*pktBytes)
	}
}
