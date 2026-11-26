package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/archive/tsrestore/internal/cache"
	"github.com/archive/tsrestore/internal/checkpoint"
	"github.com/archive/tsrestore/internal/rtp"
	"github.com/archive/tsrestore/internal/segment"
	"github.com/archive/tsrestore/internal/timeline"
)

// RenditionInput is one bitrate rung's framed datagram stream plus its labels.
type RenditionInput struct {
	Name       string
	SSRC       uint32
	Bandwidth  uint32
	Width      int
	Height     int
	Datagrams  []byte
}

// Config controls a pipeline run.
type Config struct {
	OutDir       string
	Seed         int64
	ProgramSecs  int
	Resume       bool
	// SlowConsumer, when > 0, is an artificial per-segment sink delay used to
	// demonstrate backpressure. Production callers pass zero.
	SlowConsumerDelay func()
}

// RenditionResult summarizes one rendition's restoration.
type RenditionResult struct {
	Name            string
	Segments        []*segment.Segment
	CacheHits       int
	CacheRejects    int
	Reorder         rtp.ReorderStats
	Resync          segment.ResyncStats
	Backpressured   int64
}

// Result is the aggregate outcome across renditions.
type Result struct {
	Renditions      []*RenditionResult
	PeakBufferBytes int64
}

// hashBytes returns lowercase hex SHA-256.
func hashBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Run executes the full pipeline over all rendition inputs under ctx. The same
// semaphore (16 MiB) bounds the combined in-flight memory of every rendition,
// so a slow HLS sink on one rung back-pressures the producers correctly.
func Run(ctx context.Context, in []*RenditionInput, ckpt *checkpoint.Store, cas *cache.Store, cfg Config) (*Result, error) {
	sem := newMemSemaphore(MaxInFlightBytes)
	res := &Result{}

	for _, ri := range in {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rr, err := runRendition(ctx, sem, ri, ckpt, cas, cfg)
		if err != nil {
			return nil, err
		}
		res.Renditions = append(res.Renditions, rr)
	}
	res.PeakBufferBytes = sem.PeakBytes()
	return res, nil
}

// runRendition restores one rendition end to end.
func runRendition(ctx context.Context, sem *memSemaphore, ri *RenditionInput,
	ckpt *checkpoint.Store, cas *cache.Store, cfg Config) (*RenditionResult, error) {

	rr := &RenditionResult{Name: ri.Name}
	segDir := filepath.Join(cfg.OutDir, ri.Name)
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, timeline.NewError(timeline.ClassRetryable, "pipeline.mkdir",
			"cannot create segment directory", err)
	}

	reader := rtp.NewOrderedReader(bytes.NewReader(ri.Datagrams))
	clock := timeline.NewClock(timeline.RTPClockRate)
	reader.OnSR = func(sr rtp.SenderReport) {
		_ = clock.SetSR(timeline.SRAnchor{NTPTime: sr.NTPTime, RTPTime: sr.RTPTime})
	}
	builder := segment.NewBuilder(ri.Name, clock)
	nextIndex := 0
	if cfg.Resume {
		nextIndex = ckpt.NextIndex(ri.Name)
	}

	emit := func(seg *segment.Segment) error {
		// Source digest proves what the segment bytes were derived from; the
		// CAS independently hashes the stored output.
		sourceHash := hashBytes(seg.Data)
		key := cache.Key{Rendition: ri.Name, SegmentID: seg.ID, SourceHash: sourceHash}

		if cfg.Resume {
			// Already committed in a prior run: the cache must reproduce the
			// exact committed hash or it is an integrity failure.
			if committed := ckpt.HashFor(ri.Name, seg.Index); committed != "" {
				if seg.SHA256 != committed {
					return timeline.IntegrityError("pipeline.resume",
						"resumed segment hash differs from checkpoint", nil)
				}
			}
		}

		payload, hit, gerr := cas.Get(key)
		if gerr != nil {
			if errors.Is(gerr, timeline.ErrIntegrity) {
				rr.CacheRejects++
			} else {
				return gerr
			}
		}
		if !hit {
			payload = seg.Data
			if _, perr := cas.Put(key, payload); perr != nil {
				return perr
			}
		} else {
			rr.CacheHits++
		}

		// Bound the write in the shared in-flight budget; a slow sink blocks
		// here, which is the required backpressure (no silent drops).
		size := len(payload)
		if err := sem.Acquire(ctx, size); err != nil {
			return err
		}
		defer sem.Release(size)
		if cfg.SlowConsumerDelay != nil {
			cfg.SlowConsumerDelay()
		}
		if err := writeSegment(ctx, filepath.Join(segDir, seg.ID+".ts"), payload); err != nil {
			return err
		}
		// Checkpoint only after durable write + hash verification.
		got := hashBytes(payload)
		if got != seg.SHA256 {
			return timeline.IntegrityError("pipeline.hash",
				"written segment failed output hash verification", nil)
		}
		seg.SHA256 = got
		if err := ckpt.CommitSegment(ri.Name, seg.Index, got, seg.EndTick, 0); err != nil {
			return err
		}
		rr.Segments = append(rr.Segments, seg)
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var te *timeline.Error
			if errors.As(err, &te) && te.Class == timeline.ClassCorrupt {
				// Corrupt datagram: counted via reorder stats where possible;
				// skip and keep restoring rather than aborting the program.
				continue
			}
			return nil, err
		}
		made, perr := builder.Push(p)
		if perr != nil {
			return nil, perr
		}
		for _, seg := range made {
			if seg.Index < nextIndex {
				continue // resume: skip work already durably committed
			}
			if err := emit(seg); err != nil {
				return nil, err
			}
		}
	}
	if tail := builder.Flush(); tail != nil && tail.Index >= nextIndex {
		if err := emit(tail); err != nil {
			return nil, err
		}
	}

	rr.Reorder = reader.Stats()
	rr.Resync = builder.Stats()
	return rr, nil
}

// writeSegment writes payload durably (temp + fsync + rename) so the segment is
// only ever visible once fully on disk.
func writeSegment(ctx context.Context, path string, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".seg-*")
	if err != nil {
		return timeline.NewError(timeline.ClassRetryable, "pipeline.write",
			"cannot create temp segment", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return timeline.NewError(timeline.ClassRetryable, "pipeline.write",
			"segment write failed", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return timeline.NewError(timeline.ClassRetryable, "pipeline.write",
			"segment fsync failed", err)
	}
	if err := tmp.Close(); err != nil {
		return timeline.NewError(timeline.ClassRetryable, "pipeline.write",
			"segment close failed", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return timeline.NewError(timeline.ClassRetryable, "pipeline.write",
			"segment rename failed", err)
	}
	return nil
}
