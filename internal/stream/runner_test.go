package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"avpipe/internal/media"
	"avpipe/internal/timeline"
)

var rate48 = media.Rate{Num: 48000, Den: 1}

// sliceSource feeds pre-built items and then EOF.
type sliceSource struct {
	mu    sync.Mutex
	items []SourceItem
	err   error
	i     int
}

func (s *sliceSource) Read(strict bool) (SourceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return SourceItem{}, s.err
	}
	if s.i >= len(s.items) {
		return SourceItem{}, io.EOF
	}
	it := s.items[s.i]
	s.i++
	return it, nil
}

// passProc returns the input unchanged and records a flush.
type passProc struct {
	flushed atomic.Bool
}

func (p *passProc) Process(in media.Block) ([]media.Block, error) { return []media.Block{in}, nil }
func (p *passProc) Flush() ([]media.Block, error) {
	p.flushed.Store(true)
	return nil, nil
}

// collectSink records every block and can hook the first consumption.
type collectSink struct {
	mu     sync.Mutex
	blocks []media.Block
	onEach func(media.Block)
}

func (c *collectSink) Consume(b media.Block) error {
	if c.onEach != nil {
		c.onEach(b)
	}
	c.mu.Lock()
	c.blocks = append(c.blocks, b)
	c.mu.Unlock()
	return nil
}

func (c *collectSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.blocks)
}

func newTimeline(t *testing.T, audio, video int) *timeline.Timeline {
	t.Helper()
	tl, err := timeline.New(rate48)
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.Add(timeline.Track{
		ID: "mix", Kind: timeline.Audio, Rate: rate48, Channels: 1, Labels: []string{"M"},
	}); err != nil {
		t.Fatal(err)
	}
	if video > 0 {
		if err := tl.Add(timeline.Track{ID: "cam", Kind: timeline.Video,
			Anchors: []timeline.Anchor{{At: 0}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tl.Finalize(); err != nil {
		t.Fatal(err)
	}
	return tl
}

func audioItems(n, frames int, startSel func(i int) int64) []SourceItem {
	items := make([]SourceItem, n)
	var pos int64
	for i := range items {
		st := pos
		if startSel != nil {
			st = startSel(i)
		}
		items[i] = SourceItem{Block: media.Block{
			Track: "mix", Seq: uint64(i), Start: st, Rate: rate48, Channels: 1,
			Samples: make([]float64, frames),
		}}
		pos = st + int64(frames)
	}
	return items
}

func TestRunnerStreamsEverythingWithBackpressure(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	src := &sliceSource{items: audioItems(40, 64, nil)}
	sink := &collectSink{onEach: func(media.Block) { time.Sleep(time.Millisecond) }}
	r, err := NewRunner(RunnerOptions{Capacity: 2, MaxBlock: 4096, Track: "mix", Timeline: tl})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), src, &passProc{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Consumed != 40 || res.Produced != 40 {
		t.Fatalf("produced=%d consumed=%d", res.Produced, res.Consumed)
	}
	if res.Queue.Capacity != 2 {
		t.Fatalf("capacity=%d", res.Queue.Capacity)
	}
	if res.Queue.HighWater > 2 {
		t.Fatalf("high water %d", res.Queue.HighWater)
	}
	if res.Queue.Backpressure == 0 {
		t.Fatal("expected backpressure with a slow consumer")
	}
	if res.Consumed != uint64(sink.count()) {
		t.Fatalf("consumed=%d sink=%d", res.Consumed, sink.count())
	}
}

func TestRunnerCancellationPreservesCompleteBlocks(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	src := &sliceSource{items: audioItems(200, 32, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	sink := &collectSink{onEach: func(media.Block) {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}}
	r, _ := NewRunner(RunnerOptions{Capacity: 3, MaxBlock: 4096, Track: "mix", Timeline: tl})
	res, err := r.Run(ctx, src, &passProc{}, sink)
	if err == nil {
		t.Fatal("cancelled run reported success")
	}
	if !errors.Is(err, media.ErrCancelled) {
		t.Fatalf("err=%v", err)
	}
	if !res.Canceled {
		t.Fatal("result not marked cancelled")
	}
	if res.Queue.HighWater > 3 {
		t.Fatalf("queue exceeded bound: %d", res.Queue.HighWater)
	}
	// Every block that was enqueued before cancellation must have reached the
	// sink: the queue drains rather than dropping the tail.
	if res.Consumed != res.Queue.Enqueued {
		t.Fatalf("consumed=%d enqueued=%d", res.Consumed, res.Queue.Enqueued)
	}
	got := sink.count()
	if got == 0 {
		t.Fatal("no blocks consumed")
	}
	// Blocks must remain contiguous and complete.
	for i, b := range sink.blocks {
		if b.Frames() != 32 {
			t.Fatalf("block %d truncated to %d frames", i, b.Frames())
		}
		if i > 0 && b.Start != sink.blocks[i-1].Start+32 {
			t.Fatalf("block %d start %d, previous end %d", i, b.Start, sink.blocks[i-1].Start+32)
		}
	}
	if media.ExitFor(err) != media.ExitInternal {
		t.Fatalf("cancel exit=%d", media.ExitFor(err))
	}
}

func TestRunnerDuplicatesAndConflict(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	items := audioItems(3, 16, nil)
	// Repeat block 1 verbatim: it must be dropped.
	dup := items[1]
	dupItems := append([]SourceItem{}, items...)
	dupItems = append(dupItems, dup)
	// The duplicate must appear in sequence order, so place it after block 1.
	ordered := []SourceItem{items[0], items[1], items[1], items[2]}
	r, _ := NewRunner(RunnerOptions{Capacity: 4, MaxBlock: 4096, Track: "mix", Timeline: tl})
	sink := &collectSink{}
	res, err := r.Run(context.Background(), &sliceSource{items: ordered}, &passProc{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicates != 1 {
		t.Fatalf("duplicates=%d", res.Duplicates)
	}
	if res.Consumed != 3 {
		t.Fatalf("consumed=%d want 3", res.Consumed)
	}

	// Same sequence number, different content: hard failure.
	tl2 := newTimeline(t, 1, 0)
	conflict := audioItems(3, 16, nil)
	conflict = append(conflict[:2], append([]SourceItem{conflict[1]}, conflict[2:]...)...)
	conflict[2].Block.Samples = append([]float64(nil), conflict[1].Block.Samples...)
	conflict[2].Block.Samples[0] = 0.5
	sink2 := &collectSink{}
	r2, _ := NewRunner(RunnerOptions{Capacity: 4, MaxBlock: 4096, Track: "mix", Timeline: tl2})
	res2, err := r2.Run(context.Background(), &sliceSource{items: conflict}, &passProc{}, sink2)
	if err == nil {
		t.Fatal("conflict accepted")
	}
	if !errors.Is(err, media.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
	if media.ExitFor(err) != media.ExitInput {
		t.Fatalf("exit=%d", media.ExitFor(err))
	}
	if res2.Consumed != 2 {
		t.Fatalf("blocks before conflict=%d", res2.Consumed)
	}
	_ = dupItems
}

// errSink fails on its first block; the runner must return that error rather
// than hanging with a producer parked on a full queue.
type errSink struct{ err error }

func (e errSink) Consume(media.Block) error { return e.err }

func TestRunnerSinkErrorDoesNotHang(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	src := &sliceSource{items: audioItems(200, 16, nil)}
	r, _ := NewRunner(RunnerOptions{Capacity: 2, MaxBlock: 4096, Track: "mix", Timeline: tl})
	sinkErr := errors.New("disk full")
	done := make(chan struct{})
	var err error
	go func() {
		_, err = r.Run(context.Background(), src, &passProc{}, errSink{err: sinkErr})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run hung after sink error")
	}
	if err == nil || !errors.Is(err, sinkErr) {
		t.Fatalf("err=%v want sink error", err)
	}
}

func TestRunnerLeadingGapFilled(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	items := []SourceItem{
		{Block: media.Block{Track: "mix", Seq: 0, Start: 100, Rate: rate48, Channels: 1, Samples: make([]float64, 16)}},
	}
	sink := &collectSink{}
	r, _ := NewRunner(RunnerOptions{Capacity: 4, MaxBlock: 32, MaxGapFrames: 1000, Track: "mix", Timeline: tl})
	res, err := r.Run(context.Background(), &sliceSource{items: items}, &passProc{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Gaps != 1 || res.GapFrames != 100 {
		t.Fatalf("gaps=%d frames=%d", res.Gaps, res.GapFrames)
	}
	if sink.blocks[0].Start != 0 {
		t.Fatalf("output starts at %d, expected leading silence from 0", sink.blocks[0].Start)
	}
	var total int
	for _, b := range sink.blocks {
		total += b.Frames()
	}
	if total != 116 {
		t.Fatalf("total frames=%d want 116", total)
	}

	// An oversized leading gap must be rejected just like an interior one.
	tl2 := newTimeline(t, 1, 0)
	big := []SourceItem{
		{Block: media.Block{Track: "mix", Seq: 0, Start: 1_000_000, Rate: rate48, Channels: 1, Samples: make([]float64, 16)}},
	}
	r2, _ := NewRunner(RunnerOptions{Capacity: 2, MaxGapFrames: 100, Track: "mix", Timeline: tl2})
	if _, err := r2.Run(context.Background(), &sliceSource{items: big}, &passProc{}, &collectSink{}); err == nil {
		t.Fatal("oversized leading gap accepted")
	}
}

func TestRunnerGapFillAndOversizeGap(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	nonZero := make([]float64, 16)
	for i := range nonZero {
		nonZero[i] = 0.5
	}
	items := []SourceItem{
		{Block: media.Block{Track: "mix", Seq: 0, Start: 0, Rate: rate48, Channels: 1, Samples: append([]float64(nil), nonZero...)}},
		{Block: media.Block{Track: "mix", Seq: 1, Start: 50, Rate: rate48, Channels: 1, Samples: append([]float64(nil), nonZero...)}},
	}
	sink := &collectSink{}
	r, _ := NewRunner(RunnerOptions{Capacity: 4, MaxBlock: 32, MaxGapFrames: 1000, Track: "mix", Timeline: tl})
	res, err := r.Run(context.Background(), &sliceSource{items: items}, &passProc{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Gaps != 1 || res.GapFrames != 34 {
		t.Fatalf("gaps=%d frames=%d", res.Gaps, res.GapFrames)
	}
	// The sink must see a contiguous timeline where the 50-frame gap is
	// filled with silence, and the original samples are untouched.
	var total int
	var silence int
	for i, b := range sink.blocks {
		if i > 0 && b.Start != sink.blocks[i-1].Start+int64(sink.blocks[i-1].Frames()) {
			t.Fatalf("block %d discontinuous", i)
		}
		for _, s := range b.Samples {
			if s == 0 {
				silence++
			}
		}
		total += b.Frames()
	}
	if total != 66 {
		t.Fatalf("total frames=%d want 66", total)
	}
	if silence != 34 {
		t.Fatalf("silence frames=%d want 34", silence)
	}

	tl2 := newTimeline(t, 1, 0)
	big := []SourceItem{
		{Block: media.Block{Track: "mix", Seq: 0, Start: 0, Rate: rate48, Channels: 1, Samples: make([]float64, 10)}},
		{Block: media.Block{Track: "mix", Seq: 1, Start: 5000, Rate: rate48, Channels: 1, Samples: make([]float64, 10)}},
	}
	r2, _ := NewRunner(RunnerOptions{Capacity: 4, MaxGapFrames: 100, Track: "mix", Timeline: tl2})
	if _, err := r2.Run(context.Background(), &sliceSource{items: big}, &passProc{}, &collectSink{}); err == nil {
		t.Fatal("oversize gap accepted")
	}
}

func TestRunnerRejectsReorder(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	items := audioItems(3, 10, nil)
	// Swap blocks 1 and 2 so the second block starts behind the first's end.
	items[1], items[2] = items[2], items[1]
	r, _ := NewRunner(RunnerOptions{Capacity: 4, MaxBlock: 4096, MaxGapFrames: 1000, Track: "mix", Timeline: tl})
	_, err := r.Run(context.Background(), &sliceSource{items: items}, &passProc{}, &collectSink{})
	if err == nil {
		t.Fatal("reorder accepted")
	}
	if media.ExitFor(err) != media.ExitInput {
		t.Fatalf("exit=%d", media.ExitFor(err))
	}
}

func TestRunnerFlushAndEmptyTimeline(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	proc := &passProc{}
	r, _ := NewRunner(RunnerOptions{Capacity: 2, Track: "mix", Timeline: tl})
	res, err := r.Run(context.Background(), &sliceSource{}, proc, &collectSink{})
	if err != nil {
		t.Fatal(err)
	}
	if !proc.flushed.Load() {
		t.Fatal("processor was not flushed")
	}
	if res.Produced != 0 {
		t.Fatalf("produced=%d", res.Produced)
	}
}

func TestRunnerRejectsBadInputBeforeOutput(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	src := &sliceSource{err: media.NewError(media.PhaseInput, media.ExitInput, "mix", 0, false, "read",
		fmt.Errorf("%w: bad", media.ErrCorruptInput))}
	sink := &collectSink{}
	r, _ := NewRunner(RunnerOptions{Capacity: 2, Track: "mix", Timeline: tl})
	res, err := r.Run(context.Background(), src, &passProc{}, sink)
	if err == nil || media.ExitFor(err) != media.ExitInput {
		t.Fatalf("err=%v", err)
	}
	if res.Consumed != 0 || sink.count() != 0 {
		t.Fatalf("output side effects: consumed=%d sink=%d", res.Consumed, sink.count())
	}
}

func TestRunnerOptionValidation(t *testing.T) {
	tl := newTimeline(t, 1, 0)
	if _, err := NewRunner(RunnerOptions{Timeline: tl}); err == nil {
		t.Fatal("zero capacity accepted")
	}
	if _, err := NewRunner(RunnerOptions{Capacity: 1}); err == nil {
		t.Fatal("nil timeline accepted")
	}
	if _, err := NewRunner(RunnerOptions{Capacity: 1, MaxGapFrames: -1, Timeline: tl}); err == nil {
		t.Fatal("negative gap accepted")
	}
	// MaxBlock defaults rather than failing.
	r, err := NewRunner(RunnerOptions{Capacity: 1, Timeline: tl})
	if err != nil || r.opts.MaxBlock == 0 {
		t.Fatalf("default max block missing: %v", err)
	}
}

func TestStallSink(t *testing.T) {
	sink := &collectSink{}
	var slept time.Duration
	ss := StallSink{Inner: sink, Delay: 5 * time.Millisecond, Sleep: func(d time.Duration) { slept += d }}
	if err := ss.Consume(media.Block{}); err != nil {
		t.Fatal(err)
	}
	if slept != 5*time.Millisecond || sink.count() != 1 {
		t.Fatalf("slept=%v count=%d", slept, sink.count())
	}
}
