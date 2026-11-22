package worker

import (
	"context"
	"sync"

	"mediad/internal/checkpoint"
)

// Run processes inputs. Chunks recorded in prior are skipped and counted in
// Recovered. The manifest digest must match the checkpoint, otherwise
// ErrInputChanged is returned before any work starts.
func Run(ctx context.Context, inputs []SegmentInput, opts Options, manifestDigest string, prior *checkpoint.State) (*Result, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if err := EnsureInputs(inputs); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	doneChunks := map[int]bool{}
	if prior != nil {
		done, err := checkpoint.Resume(prior, manifestDigest)
		if err != nil {
			return nil, err
		}
		for _, d := range done {
			doneChunks[d] = true
		}
	}
	totalChunks := checkpoint.ChunkCount(len(inputs), opts.ChunkSize)
	type segmentResult struct {
		out   Output
		chunk int
	}
	outByIndex := make([]segmentResult, len(inputs))
	have := make([]bool, len(inputs))
	res := &Result{ManifestSHA: manifestDigest, Chunks: totalChunks}
	if prior != nil {
		for _, o := range outputsFromState(prior) {
			if idx, ok := lookupOutput(inputs, o); ok {
				outByIndex[idx], have[idx] = segmentResult{o, o.Chunk}, true
			}
		}
	}
	sem := make(chan struct{}, opts.Workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ctxErr error
	for chunk := 0; chunk < totalChunks; chunk++ {
		start, end := checkpoint.ChunkRange(chunk, len(inputs), opts.ChunkSize)
		if doneChunks[chunk] {
			mu.Lock()
			res.Recovered++
			mu.Unlock()
			continue
		}
		for i := start; i < end; i++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				mu.Lock()
				stop := ctxErr != nil
				mu.Unlock()
				if stop {
					return
				}
				select {
				case <-ctx.Done():
					mu.Lock()
					ctxErr = ctx.Err()
					mu.Unlock()
					return
				default:
				}
				out := process(inputs[i], opts.Target, chunk)
				mu.Lock()
				outByIndex[i] = segmentResult{out: out, chunk: chunk}
				have[i] = true
				mu.Unlock()
			}(i)
		}
		wg.Wait()
		if ctxErr != nil {
			return nil, ctxErr
		}
	}
	for i := range inputs {
		if have[i] {
			res.Outputs = append(res.Outputs, outByIndex[i].out)
		}
	}
	sortOutputs(res.Outputs)
	res.Faults = append(res.Faults, crossSegmentFaults(res.Outputs)...)
	res.KeyIndex = buildIndex(res.Outputs)
	for _, o := range res.Outputs {
		res.Faults = append(res.Faults, o.Faults...)
	}
	return res, nil
}

// crossSegmentFaults detects timeline discontinuities across segment
// boundaries. The later segment is reported at its own start so recovery can
// resume from the first segment after the jump.
