package stream

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"fieldqc/internal/clock"
	"fieldqc/internal/diagnose"
	"fieldqc/internal/fault"
	"fieldqc/internal/fixture"
	"fieldqc/internal/isobmff"
)

func passThrough(delay time.Duration) Stage {
	return func(ctx context.Context, in <-chan Item, out chan<- Item) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case it, ok := <-in:
				if !ok {
					return nil
				}
				if delay > 0 {
					time.Sleep(delay)
				}
				select {
				case out <- it:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
}

func TestBackpressureBoundsBacklog(t *testing.T) {
	p := New(1, 4<<20) // 4 MiB budget
	src := make(chan Item)
	var sent, done, maxInFlight atomic.Int64
	go func() {
		defer close(src)
		for i := 0; i < 30; i++ {
			src <- Item{Window: i, Size: 1 << 20}
			n := sent.Add(1) - done.Load()
			for {
				old := maxInFlight.Load()
				if n <= old || maxInFlight.CompareAndSwap(old, n) {
					break
				}
			}
		}
	}()
	stage := func(ctx context.Context, in <-chan Item, out chan<- Item) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case it, ok := <-in:
				if !ok {
					return nil
				}
				time.Sleep(2 * time.Millisecond)
				done.Add(1)
				select {
				case out <- it:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	if err := p.Run(context.Background(), src, stage); err != nil {
		t.Fatal(err)
	}
	if got := maxInFlight.Load(); got > 12 {
		t.Fatalf("in-flight backlog = %d, want bounded (<= 12)", got)
	}
	if n := len(p.tokens); n != 0 {
		t.Fatalf("tokens leaked after run: %d", n)
	}
}

func TestCancelAbortsPipeline(t *testing.T) {
	p := New(2, 8<<20)
	src := make(chan Item, 5)
	for i := 0; i < 5; i++ {
		src <- Item{Window: i, Size: 1 << 20}
	}
	close(src)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := p.Run(ctx, src, passThrough(50*time.Millisecond), passThrough(0))
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	if n := len(p.tokens); n != 0 {
		t.Fatalf("tokens leaked after abort: %d", n)
	}
}

// TestCrossModuleSpanConsistency verifies that truncation and timestamp
// jumps are located identically across parse -> fault model -> diagnose,
// and that repeated runs produce identical digests.
func TestCrossModuleSpanConsistency(t *testing.T) {
	tracks := []fixture.MP4Track{
		{ // 60 s video with a 10x delta jump at exactly 30 s
			ID: 1, Kind: "video", Timescale: 90000,
			Deltas: []uint32{3600, 36000, 3600}, Counts: []uint32{750, 1, 749},
			Keyframes:    []uint32{1, 751},
			SampleSize:   40,
			ChunkSamples: 250,
			MediaTime:    -1,
		},
		{ // ~60 s audio
			ID: 2, Kind: "audio", Timescale: 48000,
			Deltas: []uint32{1024}, Counts: []uint32{2813},
			SampleSize:   8,
			ChunkSamples: 469,
			MediaTime:    -1,
		},
	}
	mp := fixture.BuildMP4(tracks)
	cut := mp.DataStart[1] + 4096 // truncate inside the audio mdat region
	run := func() (string, []fault.Span) {
		res, err := isobmff.Analyze("x.mp4", mp.Data[:cut])
		if err != nil {
			t.Fatal(err)
		}
		merged := diagnose.Merge(res.Faults.Spans())
		m := fault.NewModel()
		for _, s := range merged {
			m.Add(s)
		}
		return m.Digest(), merged
	}
	d1, merged := run()
	d2, _ := run()
	if d1 != d2 {
		t.Fatalf("digest not repeatable: %s vs %s", d1, d2)
	}
	wantJump, _ := clock.Ticks(750*3600, 90000) // 30 s exactly
	wantTrunc, _ := clock.Ticks(469*1024, 48000)
	wantTruncEnd, _ := clock.Ticks(2813*1024, 48000)
	var gotJump, gotTrunc bool
	for _, s := range merged {
		switch s.Code {
		case fault.CodeTimestampJump:
			if s.Track == "x.mp4#1" && s.Start.Cmp(wantJump) == 0 {
				gotJump = true
			}
		case fault.CodeTruncatedBox:
			if s.Track == "x.mp4#2" && s.Start.Cmp(wantTrunc) == 0 && s.End.Cmp(wantTruncEnd) == 0 {
				gotTrunc = true
			}
		}
	}
	if !gotJump {
		t.Fatalf("jump span not located at 30s across modules: %v", merged)
	}
	if !gotTrunc {
		t.Fatalf("truncation span not localized across modules: %v", merged)
	}
}
