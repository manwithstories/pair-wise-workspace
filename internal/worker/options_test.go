package worker

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"mediad/internal/audio"
)

func TestOptionsValidation(t *testing.T) {
	for _, o := range []Options{
		{ChunkSize: 0, Workers: 1, Target: audio.DefaultTarget()},
		{ChunkSize: 1, Workers: 0, Target: audio.DefaultTarget()},
		{ChunkSize: 1, Workers: 1, MemoryLimitBytes: -1, Target: audio.DefaultTarget()},
		{ChunkSize: 1, Workers: 1, Target: audio.Target{SampleRate: 44100, Channels: 2, LoudnessLUFS: -23, PeakCeilingDBFS: -1}},
	} {
		if err := o.Validate(); err == nil {
			t.Fatalf("expected invalid options: %+v", o)
		}
	}
	if err := DefaultOptions().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := EnsureInputs(nil); !errors.Is(err, ErrNoInputs) {
		t.Fatalf("empty inputs = %v", err)
	}
}

func TestConcurrentRunsAreSafe(t *testing.T) {
	inputs := makeInputs(12, 3)
	opts := DefaultOptions()
	opts.Workers = 6
	var wg sync.WaitGroup
	results := make([]*Result, 4)
	errs := make([]error, 4)
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			results[k], errs[k] = Run(context.Background(), inputs, opts, testDigest, nil)
		}(k)
	}
	wg.Wait()
	for k := 0; k < 4; k++ {
		if errs[k] != nil {
			t.Fatal(errs[k])
		}
	}
	for k := 1; k < 4; k++ {
		for i := range results[0].Outputs {
			if results[0].Outputs[i].OutputHash != results[k].Outputs[i].OutputHash {
				t.Fatalf("concurrent run %d differs at %d", k, i)
			}
		}
	}
}

func TestContextCancellation(t *testing.T) {
	inputs := makeInputs(64, 0)
	opts := DefaultOptions()
	opts.Workers = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, inputs, opts, testDigest, nil); err == nil {
		t.Fatal("expected cancellation error")
	}
	// A normal context still completes.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if _, err := Run(ctx2, inputs, opts, testDigest, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSortStability(t *testing.T) {
	outs := []Output{{URI: "b", Sequence: 2}, {URI: "a", Sequence: 1}, {URI: "c", Sequence: 2}}
	sortOutputs(outs)
	if outs[0].URI != "a" || outs[1].URI != "b" || outs[2].URI != "c" {
		t.Fatalf("order = %+v", outs)
	}
	_ = sort.IntsAreSorted
}
