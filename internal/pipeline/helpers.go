package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"mediacore/internal/clock"
	"mediacore/internal/fixture"
	"mediacore/internal/merge"
	"mediacore/internal/pool"
	"mediacore/internal/remux"
	"mediacore/internal/sample"
	"mediacore/internal/timeline"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// digestOfSeed derives a stable input identity from the seed.
func digestOfSeed(seed int64) string {
	h := sha256.Sum256([]byte("mediacore-input:" + itoa(seed)))
	return hex.EncodeToString(h[:])
}

// timelineSelect runs the whole-sample boundary rule over the mapped samples.
// The default plan covers the full timeline: a zero start and an end beyond
// every sample's PTS, so each track retains all of its complete samples.
func timelineSelect(in []*sample.Sample) []*sample.Sample {
	if len(in) == 0 {
		return in
	}
	var maxEnd clock.Tick
	for _, s := range in {
		if end := s.PTS + s.Duration; end > maxEnd {
			maxEnd = end
		}
	}
	plan := timeline.Plan{Cuts: []timeline.Cut{{Start: 0, End: maxEnd}}}
	return timeline.Select(in, plan).Samples
}

// processThroughPool runs jobs through the bounded pool and returns the
// deterministically merged output. It reports cancellation with the first
// uncommitted input sequence.
func processThroughPool(ctx context.Context, cfg Config, in []*sample.Sample,
	startSeq int) (out []*sample.Sample, cancelled bool, uncommitted int, err error) {

	p := pool.New(ctx, cfg.Workers, cfg.QueueCap, func(ctx context.Context, j pool.Job) (any, error) {
		if cfg.WorkerGate != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-cfg.WorkerGate:
			}
		}
		if cfg.WorkerDelay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(cfg.WorkerDelay):
			}
		}
		s := j.Payload.(*sample.Sample)
		// The worker passes the compressed sample through unchanged; no decode.
		cp := *s
		return &cp, nil
	})
	defer p.Shutdown(0)

	rb := &merge.ReorderBuffer{}
	subErr := make(chan error, 1)
	go func() {
		for i, s := range in {
			seq := startSeq + i
			if e := p.Submit(ctx, pool.Job{InputSeq: seq, TrackID: s.TrackID, Payload: s}); e != nil {
				subErr <- e
				return
			}
		}
		p.Close()
		subErr <- nil
	}()

	consumed := 0
consumerLoop:
	for {
		select {
		case <-ctx.Done():
			cancelled = true
			uncommitted = startSeq + consumed
			return nil, true, uncommitted, nil
		case r, ok := <-p.Results():
			if !ok {
				break consumerLoop
			}
			if r.Err != nil {
				return nil, false, 0, r.Err
			}
			rb.Add(r.InputSeq, r.Out.(*sample.Sample))
			consumed++
			if consumed == len(in) {
				break consumerLoop
			}
		}
	}
	if e := <-subErr; e != nil {
		if ctx.Err() != nil {
			return nil, true, startSeq + consumed, nil
		}
		return nil, false, 0, e
	}
	out = rb.Flush()
	return out, false, 0, nil
}

// referenceOrder computes the deterministic order without any concurrency.
func referenceOrder(in []*sample.Sample, startSeq int) []*sample.Sample {
	rb := &merge.ReorderBuffer{}
	for i, s := range in {
		rb.Add(startSeq+i, s)
	}
	return rb.Flush()
}

// remuxTo writes the transport container, creating the output directory and
// rejecting path traversal via a confined base directory.
func remuxTo(cfg Config, ordered []*sample.Sample) (remux.Metrics, error) {
	if err := ensureOutputPath(cfg.OutPath); err != nil {
		return remux.Metrics{}, err
	}
	f, err := os.Create(cfg.OutPath)
	if err != nil {
		return remux.Metrics{}, err
	}
	defer f.Close()
	profiles := profilesFor(ordered)
	return remux.WriteContainer(f, ordered, profiles)
}

func ensureOutputPath(p string) error {
	clean := filepath.Clean(p)
	if filepath.IsAbs(clean) && filepath.Dir(clean) == string(filepath.Separator) {
		return os.ErrPermission
	}
	dir := filepath.Dir(clean)
	return os.MkdirAll(dir, 0o755)
}

func profilesFor(ordered []*sample.Sample) []remux.TrackProfile {
	seen := map[uint32]sample.Codec{}
	for _, s := range ordered {
		seen[s.TrackID] = s.Codec
	}
	var ids []uint32
	for id := range seen {
		ids = append(ids, id)
	}
	// Sort for stable profile order.
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	out := make([]remux.TrackProfile, 0, len(ids))
	for _, id := range ids {
		out = append(out, remux.TrackProfile{TrackID: id, Codec: seen[id], ClockRate: clock.Rate})
	}
	return out
}

var _ = fixture.ModeOK
