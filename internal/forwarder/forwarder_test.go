package forwarder

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestForwardsInOrder(t *testing.T) {
	var mu sync.Mutex
	var got []uint16
	// Capacity above the packet count so a tight-loop producer cannot
	// legitimately overflow the queue; ordering is what this test asserts.
	f := New(64, func(p Packet) error {
		mu.Lock()
		got = append(got, p.Sequence)
		mu.Unlock()
		return nil
	})
	f.Start()
	for i := 0; i < 50; i++ {
		if err := f.Push(Packet{Sequence: uint16(i)}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "50 forwarded", func() bool { return f.Stats().Forwarded == 50 })
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, seq := range got {
		if seq != uint16(i) {
			t.Fatalf("out of order at %d: %d", i, seq)
		}
	}
}

func TestBackpressureDropsInsteadOfBlocking(t *testing.T) {
	// A sink that never returns blocks the consumer, filling the queue.
	release := make(chan struct{})
	f := New(4, func(Packet) error {
		<-release
		return nil
	})
	f.Start()
	defer func() {
		close(release)
		_ = f.Close()
	}()

	// Push far more than the capacity; Push must never block, so this loop
	// finishing at all is half the assertion.
	for i := 0; i < 100; i++ {
		if err := f.Push(Packet{Sequence: uint16(i)}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	st := f.Stats()
	if st.Backpressure == 0 {
		t.Fatalf("expected backpressure drops with a stuck sink, got %+v", st)
	}
	if st.QueueDepth > st.QueueCap {
		t.Fatalf("queue exceeded its bound: %+v", st)
	}
	if st.Enqueued+st.Backpressure != 100 {
		t.Fatalf("every push must be accounted for: %+v", st)
	}
}

func TestRetransmitsCountedSeparately(t *testing.T) {
	f := New(16, func(Packet) error { return nil })
	f.Start()
	_ = f.Push(Packet{Sequence: 1})
	_ = f.Push(Packet{Sequence: 1, Duplicate: true})
	waitFor(t, "one forward and one retransmit", func() bool {
		st := f.Stats()
		return st.Forwarded == 1 && st.Retransmits == 1
	})
	_ = f.Close()
}

func TestSinkErrorsAreCountedNotFatal(t *testing.T) {
	f := New(32, func(p Packet) error {
		if p.Sequence%2 == 0 {
			return errTest
		}
		return nil
	})
	f.Start()
	for i := 0; i < 10; i++ {
		_ = f.Push(Packet{Sequence: uint16(i)})
	}
	waitFor(t, "errors and forwards", func() bool {
		st := f.Stats()
		return st.Errors == 5 && st.Forwarded == 5
	})
	_ = f.Close()
}

var errTest = testError("sink failure")

type testError string

func (e testError) Error() string { return string(e) }

func TestBoundedQueueNeverGrows(t *testing.T) {
	// A deliberately slow sink with a tight-loop producer: the queue may only
	// ever hold `capacity` packets, and everything else must be backpressure.
	f := New(4, func(Packet) error {
		time.Sleep(50 * time.Microsecond)
		return nil
	})
	f.Start()
	for i := 0; i < 500; i++ {
		_ = f.Push(Packet{Sequence: uint16(i)})
	}
	st := f.Stats()
	if st.QueueDepth > 4 {
		t.Fatalf("queue grew past its bound: %+v", st)
	}
	if st.Enqueued+st.Backpressure != 500 {
		t.Fatalf("every push must be either enqueued or dropped: %+v", st)
	}
	if st.Backpressure == 0 {
		t.Fatalf("a tight producer against a slow sink must shed load: %+v", st)
	}
	_ = f.Close()
}

func TestPushAfterCloseReturnsErrClosed(t *testing.T) {
	f := New(4, func(Packet) error { return nil })
	f.Start()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Push(Packet{}); err != ErrClosed {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestCloseDrainsQueuedPackets(t *testing.T) {
	var n atomic.Int64
	f := New(64, func(Packet) error {
		time.Sleep(100 * time.Microsecond)
		n.Add(1)
		return nil
	})
	f.Start()
	for i := 0; i < 50; i++ {
		_ = f.Push(Packet{Sequence: uint16(i)})
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 50 {
		t.Fatalf("Close must drain the queue: forwarded %d of 50", got)
	}
	if st := f.Stats(); !st.Closed {
		t.Fatalf("Stats should report closed: %+v", st)
	}
}

func TestCloseIsIdempotentAndConcurrentWithPush(t *testing.T) {
	f := New(8, func(Packet) error { return nil })
	f.Start()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				// ErrClosed is expected once Close runs; anything else is a bug.
				if err := f.Push(Packet{Sequence: uint16(j)}); err != nil && err != ErrClosed {
					t.Errorf("unexpected push error: %v", err)
					return
				}
			}
		}(i)
	}
	time.Sleep(2 * time.Millisecond)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
	wg.Wait()
}

func TestZeroCapacityIsClamped(t *testing.T) {
	f := New(0, func(Packet) error { return nil })
	if cap(f.queue) != 1 {
		t.Fatalf("a zero capacity queue must be clamped to 1, got %d", cap(f.queue))
	}
}
