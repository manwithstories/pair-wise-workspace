package runner

import (
	"errors"
	"image"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"fieldcut/internal/media"
	"fieldcut/internal/render"
)

// Run executes the full pipeline. With Limit > 0 it commits at most Limit
// newly written frames and returns ErrInterrupted together with the partial
// result; the demo uses that to prove resume equals a full run.
func Run(opts Options) (*Result, error) {
	if opts.OutDir == "" {
		return nil, media.New(media.KindConflict, "runner.out", "", -1, errors.New("empty output directory"))
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 1
	}
	if workers > media.MaxTracks {
		workers = media.MaxTracks
	}
	a, err := analyze(opts)
	if err != nil {
		return nil, err
	}
	ex, _, err := render.Open(opts.OutDir, opts.ConfigHash, a.sourceHash)
	if err != nil {
		return nil, err
	}
	res := &Result{
		Frames: a.frames, SyncMaxMs: a.syncMaxMs, RecoveryRate: a.rate,
		Eligible: a.eligible, Recovered: a.recovered, Missing: a.missing,
		CorruptRepaired: a.corruptRepaired, AudioOffsetMs: a.audio.OffsetMs(),
		MissingRecords: a.missingRecords,
	}
	type done struct {
		idx int64
		dur time.Duration
	}
	// Bounded worker pool: at most Workers frames are in flight, so peak
	// memory is Workers canvases plus their decoded sources.
	jobs := make(chan int64)
	results := make(chan done, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	stop := make(chan struct{})
	newlyCommitted := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if _, ok := ex.Has(idx); ok {
					continue // committed by an earlier run: resume
				}
				start := time.Now()
				img, err := a.composeFrame(idx)
				if err == nil {
					_, err = ex.Commit(idx, img)
				}
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				select {
				case results <- done{idx: idx, dur: time.Since(start)}:
				case <-stop:
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for idx := int64(0); idx < int64(a.frames); idx++ {
			select {
			case jobs <- idx:
			case <-stop:
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	var durs []float64
	interrupted := false
	for d := range results {
		durs = append(durs, float64(d.dur.Microseconds())/1000)
		newlyCommitted++
		if opts.Limit > 0 && newlyCommitted >= opts.Limit {
			interrupted = true
			close(stop)
			break
		}
	}
	// Drain late results so no worker blocks on a full channel.
	go func() {
		for range results {
		}
	}()
	wg.Wait()
	mu.Lock()
	err = firstErr
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	res.P95FrameMs = percentile(durs, 95)
	root, err := ex.RootHash()
	if err != nil {
		return nil, err
	}
	res.Root = root
	res.Interrupted = interrupted
	if interrupted {
		return res, ErrInterrupted
	}
	own := render.Ownership{
		Width: a.width, Height: a.height,
		Config: opts.ConfigHash, Source: a.sourceHash,
	}
	if err := ex.Finalize(own); err != nil {
		return nil, err
	}
	return res, nil
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	if p >= 100 {
		return cp[len(cp)-1]
	}
	if p <= 0 {
		return cp[0]
	}
	rank := p / 100 * float64(len(cp)-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(cp) {
		return cp[len(cp)-1]
	}
	frac := rank - float64(lo)
	return cp[lo]*(1-frac) + cp[hi]*frac
}

// composeFrame builds the canvas for one output index from every track in
// paint order, loading only the frames that index needs.
func (a *analysis) composeFrame(idx int64) (*image.NRGBA, error) {
	layers := make([]render.Layer, 0, len(a.composite))
	for _, name := range a.composite {
		tf := a.tracks[name]
		if seq, ok := tf.idxSeq[idx]; ok {
			e := tf.entries[seq]
			img, err := render.LoadFrame(filepath.Join(a.inDir, e.Path), e.Width, e.Height)
			if err != nil {
				return nil, media.New(media.KindCorrupt, "render.load", name, seq, err)
			}
			layers = append(layers, render.Layer{Track: name, Seq: seq, Primary: img, Mask: tf.node.Mask})
			continue
		}
		if in, ok := tf.idxInterp[idx]; ok {
			before := tf.entries[in.Before]
			after := tf.entries[in.After]
			p, err := render.LoadFrame(filepath.Join(a.inDir, before.Path), before.Width, before.Height)
			if err != nil {
				return nil, media.New(media.KindCorrupt, "render.load", name, in.Before, err)
			}
			q, err := render.LoadFrame(filepath.Join(a.inDir, after.Path), after.Width, after.Height)
			if err != nil {
				return nil, media.New(media.KindCorrupt, "render.load", name, in.After, err)
			}
			layers = append(layers, render.Layer{
				Track: name, Seq: in.Seq, Primary: p, Secondary: q, Mix: in.Weight, Mask: tf.node.Mask,
			})
			continue
		}
		layers = append(layers, render.Layer{Track: name, Seq: idx, Mask: tf.node.Mask})
	}
	img, _, err := render.Composite(a.width, a.height, layers)
	if err != nil {
		return nil, err
	}
	return img, nil
}
