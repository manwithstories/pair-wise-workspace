package pipeline

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"avlab/internal/media"
)

func TestPoolHardLimits(t *testing.T) {
	ctx := context.Background()
	if _, err := NewPool(ctx, 0, 8); !media.IsCode(err, media.CodeInvalidArgument) {
		t.Fatalf("zero workers err = %v", err)
	}
	if _, err := NewPool(ctx, media.MaxWorkers+1, 8); !media.IsCode(err, media.CodeLimitExceeded) {
		t.Fatalf("worker cap err = %v", err)
	}
	if _, err := NewPool(ctx, 2, media.MaxQueue+1); !media.IsCode(err, media.CodeLimitExceeded) {
		t.Fatalf("queue cap err = %v", err)
	}
}

func TestBoundedInflightAndQueue(t *testing.T) {
	const workers, queue, jobs = 4, 64, 200
	ctx := context.Background()
	pool, err := NewPool(ctx, workers, queue)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var mu sync.Mutex
	cur, peak := 0, 0
	merger := NewMerger()
	type collectOut struct {
		ordered []Result
		err     error
	}
	outCh := make(chan collectOut, 1)
	go func() {
		ordered, err := merger.Collect(ctx, pool.Results())
		outCh <- collectOut{ordered, err}
	}()
	submitErr := make(chan error, 1)
	go func() {
		for i := 0; i < jobs; i++ {
			i := i
			submitErr <- pool.Submit(Job{
				Epoch: 0, Segment: uint32(i / 8), Seq: uint16(i),
				Fn: func(ctx context.Context) (any, error) {
					mu.Lock()
					cur++
					if cur > peak {
						peak = cur
					}
					mu.Unlock()
					<-gate
					mu.Lock()
					cur--
					mu.Unlock()
					return i, nil
				},
			})
		}
		close(submitErr)
	}()
	// let the pool reach steady state (workers parked on gate, queue filling)
	time.Sleep(80 * time.Millisecond)
	close(gate)
	for err := range submitErr {
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	pool.Close()
	out := <-outCh
	if out.err != nil {
		t.Fatal(out.err)
	}
	ordered := out.ordered
	if len(ordered) != jobs {
		t.Fatalf("ordered results = %d, want %d", len(ordered), jobs)
	}
	st := pool.Stats()
	if st.MaxInflight > workers {
		t.Fatalf("max inflight = %d, bound %d", st.MaxInflight, workers)
	}
	if peak > workers {
		t.Fatalf("observed concurrency = %d, bound %d", peak, workers)
	}
	if st.QueuePeak > queue {
		t.Fatalf("queue peak = %d, bound %d", st.QueuePeak, queue)
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Seq >= ordered[i].Seq {
			t.Fatalf("merger order violated at %d: %d then %d",
				i, ordered[i-1].Seq, ordered[i].Seq)
		}
	}
}

func TestCancellationDrainsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool, err := NewPool(ctx, 4, 16)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		_ = pool.Submit(Job{
			Epoch: 1, Segment: 1, Seq: uint16(i),
			Fn: func(ctx context.Context) (any, error) {
				started <- struct{}{}
				<-ctx.Done()
				return nil, media.WrapFault(media.CodeCanceled, true,
					"job canceled", ctx.Err())
			},
		})
	}
	for i := 0; i < 4; i++ {
		<-started
	}
	cancel()
	pool.Close() // must return: all workers observed ctx and exited
	st := pool.Stats()
	if st.MaxInflight > 4 {
		t.Fatalf("inflight = %d", st.MaxInflight)
	}
}

// hashJob is the deterministic per-segment work used for repeatability.
func hashJob(seg uint32, payload []byte) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		sum := sha256.Sum256(append([]byte{byte(seg)}, payload...))
		return fmt.Sprintf("%x", sum), nil
	}
}

func TestMergeDeterminismAcrossShuffledCompletion(t *testing.T) {
	run := func(shuffle bool) []Result {
		pool, err := NewPool(context.Background(), 8, 128)
		if err != nil {
			t.Fatal(err)
		}
		seqs := make([]int, 96)
		for i := range seqs {
			seqs[i] = i
		}
		if shuffle {
			// reverse submission order: completions arrive out of order
			for i, j := 0, len(seqs)-1; i < j; i, j = i+1, j-1 {
				seqs[i], seqs[j] = seqs[j], seqs[i]
			}
		}
		mg := NewMerger()
		type out struct {
			rs  []Result
			err error
		}
		outCh := make(chan out, 1)
		go func() {
			rs, err := mg.Collect(context.Background(), pool.Results())
			outCh <- out{rs, err}
		}()
		for _, i := range seqs {
			if err := pool.Submit(Job{
				Epoch: media.Epoch(i / 32), Segment: uint32(i / 8),
				Seq: uint16(i), Fn: hashJob(uint32(i), []byte("segment")),
			}); err != nil {
				t.Fatalf("submit %d: %v", i, err)
			}
		}
		pool.Close()
		res := <-outCh
		if res.err != nil {
			t.Fatal(res.err)
		}
		return res.rs
	}
	a := run(false)
	b := run(true)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Value != b[i].Value || a[i].Epoch != b[i].Epoch ||
			a[i].Segment != b[i].Segment || a[i].Seq != b[i].Seq {
			t.Fatalf("position %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestMergerRejectsDuplicates(t *testing.T) {
	mg := NewMerger()
	r := Result{Epoch: 0, Segment: 1, Seq: 5, Value: "x"}
	if err := mg.Add(r); err != nil {
		t.Fatal(err)
	}
	if err := mg.Add(r); !media.IsCode(err, media.CodePacketDuplicate) {
		t.Fatalf("duplicate err = %v", err)
	}
	if mg.Stats().Duplicates != 1 {
		t.Fatalf("dup stats = %+v", mg.Stats())
	}
}
