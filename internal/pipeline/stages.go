package pipeline

import (
	"context"
	"sync"

	"archivetl/internal/clock"
	"archivetl/internal/diag"
	"archivetl/internal/mp4"
	"archivetl/internal/subtitle"
	"archivetl/internal/timeline"
)

func produceRecords(ctx context.Context, cancel context.CancelFunc,
	sources []SubtitleSource, out chan<- recordEnvelope, gate *memGate,
	coll *diag.Collector, wg *sync.WaitGroup, errCh chan<- error) {
	defer wg.Done()
	defer close(out)
	for _, src := range sources {
		parser := subtitle.NewParser(src.Reader, src.Source, coll)
		err := parser.Parse(ctx, func(rec subtitle.Record) error {
			rec.Track = src.Track
			size := int64(len(rec.Text)) + recordOverheadBytes
			if err := gate.acquire(ctx, size); err != nil {
				return err
			}
			select {
			case out <- recordEnvelope{rec: rec, size: size}:
				return nil
			case <-ctx.Done():
				gate.release(size)
				return ctx.Err()
			}
		})
		if err != nil {
			if ctx.Err() == nil {
				errCh <- err
				cancel()
			}
			return
		}
	}
}

func windowSegments(ctx context.Context, cancel context.CancelFunc,
	in <-chan recordEnvelope, out chan<- *Segment, gate *memGate,
	cfg Config, wg *sync.WaitGroup, errCh chan<- error) {
	defer wg.Done()
	defer close(out)
	open := map[int]*Segment{}
	var order []int

	emit := func(seg *Segment) bool {
		select {
		case out <- seg:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for env := range in {
		id := int(env.rec.Start / cfg.SegmentTicks)
		seg := open[id]
		if seg == nil {
			seg = &Segment{ID: id, Start: clock.Tick(id) * cfg.SegmentTicks,
				End: clock.Tick(id+1) * cfg.SegmentTicks}
			open[id] = seg
			order = append(order, id)
		}
		seg.Records = append(seg.Records, env.rec)
		gate.release(env.size) // bytes now live inside this stage's window
	}
	if ctx.Err() != nil {
		return
	}
	sortInts(order)
	for _, id := range order {
		if !emit(open[id]) {
			errCh <- ctx.Err()
			cancel()
			return
		}
	}
}

func renderStage(ctx context.Context, cancel context.CancelFunc,
	in <-chan *Segment, out chan<- renderedEnvelope,
	cfg Config, renderer Renderer, wg *sync.WaitGroup, errCh chan<- error) {
	defer wg.Done()
	defer close(out)
	for seg := range in {
		items := make([]timeline.Item, 0, len(seg.Records))
		for _, rec := range seg.Records {
			items = append(items, timeline.Item{
				Track: rec.Track, Source: rec.Source, Sequence: rec.Sequence,
				Offset: rec.Offset, Start: rec.Start, End: rec.End, Text: rec.Text,
			})
		}
		lanes, summary := timeline.Merge(items, cfg.Horizon)
		seg.Lanes, seg.Summary = lanes, summary
		for _, f := range summary.Failures {
			seg.FailedOffsets = append(seg.FailedOffsets, f.Offset)
		}
		if cfg.Media != nil {
			seg.KeyHits = countKeyHits(cfg.Media, seg)
		}
		payload, hash, err := renderer.Render(ctx, seg)
		if err != nil {
			errCh <- &RunError{Kind: KindIO, Inner: err}
			cancel()
			return
		}
		seg.OutputHash = hash
		select {
		case out <- renderedEnvelope{seg: seg, payload: payload, hash: hash}:
		case <-ctx.Done():
			errCh <- ctx.Err()
			cancel()
			return
		}
	}
}

func sinkStage(ctx context.Context, cancel context.CancelFunc,
	in <-chan renderedEnvelope, results chan<- Result,
	cfg Config, wg *sync.WaitGroup, errCh chan<- error) {
	defer wg.Done()
	defer close(results)
	for env := range in {
		seg := env.seg
		if cfg.SlowConsumer != nil {
			cfg.SlowConsumer() // injection point for slow-archive simulation
		}
		if err := ctx.Err(); err != nil {
			errCh <- err
			cancel()
			return
		}
		match, err := cfg.Output.HasMatchingOutput(seg.ID, seg.InputHash)
		if err != nil {
			errCh <- &RunError{Kind: KindIO, Inner: err}
			cancel()
			return
		}
		if match {
			seg.Skipped = true
			select {
			case results <- Result{Segment: seg}:
			case <-ctx.Done():
				errCh <- ctx.Err()
				cancel()
				return
			}
			continue
		}
		if err := cfg.Output.WriteSegment(seg, env.payload); err != nil {
			errCh <- &RunError{Kind: KindIO, Inner: errSinkFailed}
			cancel()
			return
		}
		select {
		case results <- Result{Segment: seg}:
		case <-ctx.Done():
			errCh <- ctx.Err()
			cancel()
			return
		}
	}
}

func countKeyHits(idx *mp4.Index, seg *Segment) int {
	hits := 0
	for _, lane := range seg.Lanes {
		for _, ev := range lane.Events {
			if _, ok := idx.LookupKeyAt(ev.Start); ok {
				hits++
			}
		}
	}
	return hits
}

func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1] > v[j]; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}
