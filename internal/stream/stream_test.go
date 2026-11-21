package stream

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"mediamerge/internal/fault"
)

func block(seg, track int, payloadSize int) *Block {
	data := []byte(fmt.Sprintf("segment-%04d-track-%d", seg, track))
	for len(data) < payloadSize {
		data = append(data, byte(seg))
	}
	return &Block{
		Segment: seg, TrackID: track, Offset: int64(seg * payloadSize),
		Packet: NewPacket(uint16(seg), uint32(seg*1024), 7, data),
		Digest: sha256.Sum256(data),
	}
}

func TestQueueRejectsBadBudget(t *testing.T) {
	if _, err := NewQueue(0); err == nil {
		t.Fatal("zero budget")
	}
	if err := (&Queue{budget: 10}).Push(context.Background(), nil); err == nil {
		t.Fatal("nil block")
	}
}

func TestQueuePushPopFIFO(t *testing.T) {
	q, err := NewQueue(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		if err := q.Push(ctx, block(i, 1, 100)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 5; i++ {
		b, err := q.Pop(ctx)
		if err != nil || b == nil || b.Segment != i {
			t.Fatalf("pop %d: %+v %v", i, b, err)
		}
	}
}

func TestQueueOversizedBlockFails(t *testing.T) {
	q, _ := NewQueue(256)
	err := q.Push(context.Background(), block(1, 1, 1024))
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeQueueFull {
		t.Fatalf("bad error %v", err)
	}
}

func TestQueueBackpressureDoesNotDrop(t *testing.T) {
	// Budget for two blocks; a slow consumer forces the producer to wait.
	q, err := NewQueue(1000)
	if err != nil {
		t.Fatal(err)
	}
	const total = 12
	start := make(chan struct{})
	var prodErr error
	go func() {
		<-start
		for i := 1; i <= total; i++ {
			if err := q.Push(context.Background(), block(i, 1, 300)); err != nil {
				prodErr = err
				break
			}
		}
		q.Close()
	}()
	go func() { start <- struct{}{} }()

	sink, err := Consume(context.Background(), q, ConsumerOptions{Delay: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if prodErr != nil {
		t.Fatal(prodErr)
	}
	if sink.Blocks != total {
		t.Fatalf("consumed %d of %d blocks", sink.Blocks, total)
	}
	st := q.Stats()
	if st.Blocked == 0 {
		t.Fatal("expected producer to block on a full queue")
	}
	if st.HighWater > st.Budget {
		t.Fatalf("high water %d exceeds budget %d", st.HighWater, st.Budget)
	}
	if st.Bytes != 0 || st.Items != 0 {
		t.Fatalf("queue not drained: %+v", st)
	}
	for i, d := range sink.Digests {
		if d != block(i+1, 1, 300).Digest {
			t.Fatalf("segment %d digest mismatch", i+1)
		}
	}
}

func TestQueueCancelUnblocksProducerAndConsumer(t *testing.T) {
	q, _ := NewQueue(800)
	ctx, cancel := context.WithCancel(context.Background())
	// Fill the queue completely: two 364-byte blocks just fit.
	for i := 0; i < 2; i++ {
		if err := q.Push(context.Background(), block(i+1, 1, 300)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, block(9, 1, 300)) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		var fe *fault.Error
		if !errors.As(err, &fe) || fe.Code != fault.CodeCanceled {
			t.Fatalf("bad push error %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer stayed blocked after cancel")
	}
}

func TestRunPipeReleasesGoroutines(t *testing.T) {
	q, _ := NewQueue(2048)
	var produced int
	sink, err := RunPipe(context.Background(), q, func(ctx context.Context) error {
		for i := 1; i <= 30; i++ {
			produced++
			if err := q.Push(ctx, block(i, 1, 200)); err != nil {
				return err
			}
		}
		return nil
	}, ConsumerOptions{Delay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if produced != 30 || sink.Blocks != 30 {
		t.Fatalf("produced %d consumed %d", produced, sink.Blocks)
	}
}

func TestRunPipeProducerErrorStopsConsumer(t *testing.T) {
	q, _ := NewQueue(4096)
	boom := fault.New(fault.StageStream, fault.CodeWriteFailed, "boom")
	sink, err := RunPipe(context.Background(), q, func(ctx context.Context) error {
		for i := 1; i <= 5; i++ {
			if err := q.Push(ctx, block(i, 1, 100)); err != nil {
				return err
			}
		}
		return boom
	}, ConsumerOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("lost producer error: %v", err)
	}
	if sink == nil {
		t.Fatal("sink missing")
	}
}

func TestConsumeWriteFailureIsLocated(t *testing.T) {
	q, _ := NewQueue(1 << 16)
	for i := 1; i <= 5; i++ {
		q.Push(context.Background(), block(i, 1, 100))
	}
	q.Close()
	sink, err := Consume(context.Background(), q, ConsumerOptions{FailAfter: 3})
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeWriteFailed {
		t.Fatalf("bad error %v", err)
	}
	if sink.Blocks != 2 {
		t.Fatalf("consumed %d", sink.Blocks)
	}
	if !errors.As(sink.FailWrite, &fe) {
		t.Fatal("sink should keep the failure")
	}
	if want := "segment 3"; !contains(fe.Detail, want) {
		t.Fatalf("detail %q lacks %q", fe.Detail, want)
	}
}

func TestQueueCloseDrainsRemaining(t *testing.T) {
	q, _ := NewQueue(1 << 16)
	q.Push(context.Background(), block(1, 1, 10))
	q.Close()
	if err := q.Push(context.Background(), block(2, 1, 10)); err == nil {
		t.Fatal("push after close must fail")
	}
	b, err := q.Pop(context.Background())
	if err != nil || b == nil || b.Segment != 1 {
		t.Fatalf("drain: %+v %v", b, err)
	}
	b, err = q.Pop(context.Background())
	if err != nil || b != nil {
		t.Fatalf("expected clean EOF, got %+v %v", b, err)
	}
}

func TestNoGoroutineLeakWithFullQueueAndCancel(t *testing.T) {
	q, _ := NewQueue(500)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	var pushErr, popErr error
	go func() {
		defer wg.Done()
		for i := 1; i <= 100; i++ {
			if err := q.Push(ctx, block(i, 1, 300)); err != nil {
				pushErr = err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		_, popErr = Consume(ctx, q, ConsumerOptions{Delay: 50 * time.Millisecond})
	}()
	time.Sleep(15 * time.Millisecond)
	cancel()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutines still blocked after cancel")
	}
	if pushErr == nil || popErr == nil {
		t.Fatalf("both sides should observe cancellation: push=%v pop=%v", pushErr, popErr)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
