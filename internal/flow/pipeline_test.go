package flow_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mediaroute/internal/clock"
	"mediaroute/internal/flow"
)

func job(camera string, bitrate uint32, seg int, start int64, payload int) flow.Job {
	return flow.Job{
		Camera: camera, Bitrate: bitrate, Segment: seg,
		StartClock: start, EndClock: start + 60*clock.Rate,
		Payload:   make([]byte, payload),
	}
}

func TestQueueByteCapAndBackpressure(t *testing.T) {
	q := flow.NewQueue("q", 4096)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // slow consumer: 1 ms per job
		defer wg.Done()
		for {
			if _, err := q.Get(); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < 40; i++ {
		if err := q.Put(job("cam0", 100, i, int64(i)*60*clock.Rate, 1024)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	peak, bp := q.Stats()
	q.Close(nil)
	wg.Wait()
	if peak > 4096 {
		t.Fatalf("peak=%d exceeds cap 4096", peak)
	}
	if bp == 0 {
		t.Fatal("expected backpressure events with a slow consumer")
	}
}

func TestQueueCancelReleasesProducers(t *testing.T) {
	q := flow.NewQueue("q", 1024)
	ctx, cancel := context.WithCancel(context.Background())
	stop := q.WatchCancel(ctx)
	defer stop()
	if err := q.Put(job("cam0", 100, 0, 0, 1024)); err != nil { // fills the cap
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- q.Put(job("cam0", 100, 1, 60*clock.Rate, 1024)) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Put was not released by cancellation")
	}
	if _, err := q.Get(); err != nil { // queued items stay drainable
		t.Fatalf("drain err=%v", err)
	}
	if _, err := q.Get(); !errors.Is(err, context.Canceled) {
		t.Fatalf("get err=%v want context.Canceled", err)
	}
}

func TestFanInDeliversAlignedGroups(t *testing.T) {
	fi := flow.NewFanIn(2*clock.Rate, 3)
	var d *flow.SegmentDecision
	for tier, br := range []uint32{100, 200, 400} {
		r := flow.ChunkResult{
			Job:     job("cam0", br, 3, 180*clock.Rate, 100),
			OutPath: "/tmp/x", SHA256: "s",
		}
		_ = tier
		if got := fi.Add(r); got != nil {
			d = got
		}
	}
	if d == nil {
		t.Fatal("no decision after complete group")
	}
	if d.Mismatch || len(d.Delivered) != 3 || len(d.Quarantined) != 0 {
		t.Fatalf("decision=%+v", d)
	}
}

func TestFanInQuarantinesDeviatingRepresentation(t *testing.T) {
	fi := flow.NewFanIn(2*clock.Rate, 3)
	starts := []int64{180 * clock.Rate, 180 * clock.Rate, 186 * clock.Rate}
	var d *flow.SegmentDecision
	for i, br := range []uint32{100, 200, 400} {
		got := fi.Add(flow.ChunkResult{
			Job: job("cam0", br, 3, starts[i], 100), OutPath: "/tmp/x",
		})
		if got != nil {
			d = got
		}
	}
	if d == nil || !d.Mismatch {
		t.Fatalf("decision=%+v want mismatch", d)
	}
	if len(d.Quarantined) != 1 || d.Quarantined[0].Job.Bitrate != 400 {
		t.Fatalf("quarantined=%+v want the 400kbps chunk", d.Quarantined)
	}
	if len(d.Delivered) != 2 {
		t.Fatalf("delivered=%d want 2", len(d.Delivered))
	}
	if d.Deviation != 6*clock.Rate {
		t.Fatalf("deviation=%d want 6s", d.Deviation)
	}
}

func TestFanInFlushIncompleteGroups(t *testing.T) {
	fi := flow.NewFanIn(2*clock.Rate, 3)
	fi.Add(flow.ChunkResult{Job: job("cam1", 100, 0, 0, 100), OutPath: "/tmp/a"})
	fi.Add(flow.ChunkResult{Job: job("cam1", 200, 0, 0, 100), OutPath: "/tmp/b"})
	out := fi.Flush()
	if len(out) != 1 || len(out[0].Delivered) != 2 || out[0].Mismatch {
		t.Fatalf("flush=%+v", out)
	}
	if len(fi.Flush()) != 0 {
		t.Fatal("flush must drain groups")
	}
}
