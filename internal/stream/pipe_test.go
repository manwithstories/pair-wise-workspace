package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mediamerge/internal/model"
)

func sample(i int) model.Sample {
	return model.Sample{TrackIndex: 0, TrackID: 1, Index: i, Offset: int64(i), Data: make([]byte, 16)}
}

func TestCapacityOnePreservesAllSamples(t *testing.T) {
	p := New(context.Background(), 1)
	const n = 50
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := p.Send(sample(i)); err != nil {
				t.Errorf("send %d: %v", i, err)
				return
			}
		}
		p.CloseSend()
	}()
	got := 0
	for {
		s, ok, err := p.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if !ok {
			break
		}
		if s.Index != got {
			t.Fatalf("order: got %d want %d", s.Index, got)
		}
		got++
	}
	wg.Wait()
	if got != n {
		t.Fatalf("got %d samples", got)
	}
	if st := p.Stats(); st.Received != n || st.Queued != 0 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestSlowConsumerAppliesBackpressureWithoutLoss(t *testing.T) {
	p := New(context.Background(), 4)
	const n = 40
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			if err := p.Send(sample(i)); err != nil {
				t.Errorf("send: %v", err)
				return
			}
			if st := p.Stats(); st.Queued > st.Capacity {
				t.Errorf("queue overflow: %+v", st)
			}
		}
		p.CloseSend()
	}()
	seen := 0
	for {
		_, ok, err := p.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if !ok {
			break
		}
		seen++
		time.Sleep(time.Millisecond)
		if st := p.Stats(); st.Resident > st.Capacity+2 {
			t.Fatalf("resident %d exceeds capacity+2 (%d)", st.Resident, st.Capacity)
		}
	}
	<-done
	if seen != n {
		t.Fatalf("received %d", seen)
	}
}

func TestConsumerExitReportsBoundary(t *testing.T) {
	p := New(context.Background(), 1)
	if err := p.Send(sample(0)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Recv(); err != nil || !ok {
		t.Fatalf("recv: %v %v", ok, err)
	}
	p.SetBoundary(model.Boundary{SampleIndex: 0, TrackID: 1, TrackSample: 0, OutputBytes: 16})
	p.Fail(model.NewError(model.StageStream, model.CodeConsumerGone, 16, 1, true, "consumer exited"))
	err := p.Send(sample(1))
	if err == nil {
		t.Fatal("expected send failure after consumer exit")
	}
	var pe *model.Error
	if !errors.As(err, &pe) || pe.Code != model.CodeConsumerGone {
		t.Fatalf("err=%v", err)
	}
	if b := p.Boundary(); b.OutputBytes != 16 || b.TrackID != 1 {
		t.Fatalf("boundary=%+v", b)
	}
}

func TestCancelUnblocksSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := New(ctx, 1)
	if err := p.Send(sample(0)); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- p.Send(sample(1)) }()
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
		var pe *model.Error
		if !errors.As(err, &pe) || pe.Code != model.CodeCanceled {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not unblock")
	}
}
