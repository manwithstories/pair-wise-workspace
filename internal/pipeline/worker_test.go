package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"relaycheck/internal/model"
)

func TestOrderedProcessing(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var got []int
	p, _ := New(ctx, 2, func(r Record) {
		mu.Lock()
		got = append(got, r.Seq)
		mu.Unlock()
	})
	for i := 0; i < 10; i++ {
		if err := p.Submit(ctx, Record{Kind: KindFragment, Seq: i}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	p.Close()
	if len(got) != 10 {
		t.Fatalf("processed = %d", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("out of order at %d: %d", i, v)
		}
	}
	enq, proc, dropped := p.Stats().Snapshot()
	if enq != 10 || proc != 10 || dropped != 0 {
		t.Fatalf("stats enq=%d proc=%d dropped=%d", enq, proc, dropped)
	}
}

func TestBoundedBackpressure(t *testing.T) {
	// 容量 1，消费者先阻塞不放行；第二次 Submit 必须被背压卡住。
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	p, _ := New(context.Background(), 1, func(r Record) {
		once.Do(func() { close(started) })
		<-release
	})
	ctx := context.Background()
	if err := p.Submit(ctx, Record{Seq: 0}); err != nil {
		t.Fatal(err)
	}
	<-started // 消费者已取走第 0 条并阻塞，队列空出一个槽

	// 填满唯一槽位后，下一次提交应阻塞。
	if err := p.Submit(ctx, Record{Seq: 1}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Submit(ctx, Record{Seq: 2}) }()
	select {
	case <-done:
		t.Fatal("third submit must block under backpressure")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("blocked submit should resolve: %v", err)
	}
	p.Close()
}

func TestCancelReleasesWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	processed := make(chan struct{}, 1)
	p, pctx := New(ctx, 1, func(r Record) {
		processed <- struct{}{}
	})
	if err := p.Submit(pctx, Record{Seq: 0}); err != nil {
		t.Fatal(err)
	}
	<-processed
	cancel()

	// 取消后 Submit 立即返回 Canceled，且不得新增处理。
	if err := p.Submit(pctx, Record{Seq: 1}); err != context.Canceled {
		t.Fatalf("err = %v, want Canceled", err)
	}
	done := make(chan struct{})
	go func() { p.Cancel(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker goroutine not released after cancel")
	}
	// 取消的记录计入 dropped。
	if _, _, dropped := p.Stats().Snapshot(); dropped == 0 {
		t.Fatal("cancelled record should be counted as dropped")
	}
}

func TestParentCancelPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, pctx := New(ctx, 4, func(r Record) {})
	cancel() // 父 ctx 取消须传播到流水线内部
	if err := p.Submit(pctx, Record{Seq: 0}); err != context.Canceled {
		t.Fatalf("err = %v, want Canceled", err)
	}
	p.Cancel()
}

func TestRecordCarriesModelReports(t *testing.T) {
	p, _ := New(context.Background(), 1, func(r Record) {
		if r.Kind != KindFragment || r.Fragment.Seq != 3 {
			t.Fatalf("bad record: %+v", r)
		}
		if len(r.Fragment.Samples) != 1 {
			t.Fatal("samples not carried")
		}
	})
	_ = p.Submit(context.Background(), Record{
		Kind: KindFragment, Seq: 3,
		Fragment: model.FragmentReport{Seq: 3, Samples: []model.SampleInfo{{Index: 0}}},
	})
	p.Close()
}
