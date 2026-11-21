package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"tsbatch/internal/ts"
)

func makeJob(seq, first uint64, n int) Job {
	j := Job{Seq: seq, FirstPacket: first}
	for i := 0; i < n; i++ {
		var p [ts.PacketSize]byte
		p[0] = ts.SyncByte
		p[1] = 0x00
		p[2] = 0x20
		p[3] = 0x10
		j.Packets = append(j.Packets, p)
	}
	return j
}

func TestPoolProcessesAllJobs(t *testing.T) {
	ctx := context.Background()
	p := NewPool(ctx, 4, 8)
	const jobs = 40
	go func() {
		for i := 0; i < jobs; i++ {
			if err := p.Submit(makeJob(uint64(i), uint64(i*3), 3)); err != nil {
				t.Error(err)
			}
		}
		p.Close()
	}()
	seen := 0
	for r := range p.Results() {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		if len(r.Events) != 3 {
			t.Fatalf("events=%d", len(r.Events))
		}
		seen++
	}
	if seen != jobs {
		t.Fatalf("seen=%d want %d", seen, jobs)
	}
	if p.Peak() > 8 {
		t.Fatalf("peak=%d exceeds queue capacity", p.Peak())
	}
}

func TestMergerOrdersAndRecordsFirstError(t *testing.T) {
	m := NewMerger()
	errBoom := errors.New("boom")
	if out := m.Add(Result{Seq: 2}); len(out) != 0 {
		t.Fatal("gap must block emission")
	}
	m.Add(Result{Seq: 1, Err: errBoom})
	m.Add(Result{Seq: 3, Err: errors.New("second")})
	out := m.Add(Result{Seq: 0})
	if len(out) != 4 {
		t.Fatalf("emitted=%d want 4", len(out))
	}
	for i, r := range out {
		if r.Seq != uint64(i) {
			t.Fatalf("out of order at %d: %d", i, r.Seq)
		}
	}
	if m.FirstErr() != errBoom {
		t.Fatal("first error must be recorded")
	}
}

func TestCancellationDrainsPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := NewPool(ctx, 4, 2)
	// Fill the queue without consuming results so workers block.
	blocked := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			if err := p.Submit(makeJob(uint64(i), 0, 1)); err != nil {
				blocked <- err
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-blocked:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("submit must unblock with ctx error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not wake the blocked submitter")
	}
	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not drain after cancel")
	}
}

func TestOrderedEmitsInSequence(t *testing.T) {
	ctx := context.Background()
	in := make(chan Result, 4)
	out := Ordered(ctx, in)
	in <- Result{Seq: 1}
	in <- Result{Seq: 0}
	in <- Result{Seq: 2}
	close(in)
	want := uint64(0)
	for r := range out {
		if r.Seq != want {
			t.Fatalf("seq=%d want %d", r.Seq, want)
		}
		want++
	}
	if want != 3 {
		t.Fatalf("emitted %d results", want)
	}
}
