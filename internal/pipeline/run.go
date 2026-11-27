// Package pipeline drives the relay audit stages: parse, transform and
// verify, in that order. It propagates cancellation, commits checkpoints only
// for validated output and never marks an incomplete run as successful.
package pipeline

import (
	"context"
	"fmt"
	"time"

	"relayaudit/internal/checkpoint"
	"relayaudit/internal/fault"
	"relayaudit/internal/timeline"
	"relayaudit/internal/transform"
	"relayaudit/internal/verify"
	"relayaudit/internal/wire"
)

// DefaultBatchRows is how many source records are committed per checkpoint.
// Run executes the pipeline. A returned error is always classified: faults of
// kind Canceled mean cancellation, ErrInterrupted means resume is possible,
// and remaining errors are unrecoverable input or storage failures.
func Run(ctx context.Context, cfg Config) (*Outcome, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("pipeline: checkpoint store is required")
	}
	if len(cfg.Source) == 0 {
		return nil, fault.New(fault.Truncated, 0, "empty source segment")
	}
	var resumeClock time.Time
	if cfg.Resume {
		resumeClock = time.Now()
	}
	batch := cfg.BatchRows
	if batch <= 0 {
		batch = DefaultBatchRows
	}
	sourceHash := cfg.SourceSHA256
	if sourceHash == "" {
		sourceHash = verify.SHA256Hex(cfg.Source)
	}

	// --- parse stage -----------------------------------------------------
	scan, err := wire.Scan(cfg.Source)
	if err != nil {
		return nil, err
	}
	plan := timeline.SamplePlan(scan.Records)
	res, boundaryFaults, err := timeline.Resolve(scan.Records, plan)
	if err != nil {
		return nil, err
	}
	ranges := transform.BuildRanges(append(append([]*fault.Fault{}, scan.Damage...), boundaryFaults...), scan.SourceSize)
	transform.Annotate(ranges, res)

	out := &Outcome{
		Scan: scan, Timeline: res, BoundaryFaults: boundaryFaults,
		Ranges: ranges, IndexRebuilt: scan.IndexRebuilt,
	}

	// A finished checkpoint is returned as-is: repeat runs must not
	// re-count records or change the committed digest.
	if cfg.Resume {
		if prev, ok, err := cfg.Store.Load(); err != nil {
			return out, err
		} else if ok && prev.Done {
			if prev.SourceSHA256 != sourceHash {
				return out, fault.New(fault.Checkpoint, 0,
					"checkpoint belongs to a different source")
			}
			body, err := cfg.Store.ReadOutput()
			if err != nil {
				return out, fault.Wrap(fault.Checkpoint, 0, err, "read committed output: %v", err)
			}
			if verify.SHA256Hex(body) != prev.OutputSHA256 {
				return out, fault.New(fault.Verify, 0, "committed output no longer matches its digest")
			}
			out.Output = body
			out.OutputSHA256 = prev.OutputSHA256
			out.Kept, out.Dropped = prev.Kept, prev.Dropped
			out.NextIndex = prev.NextIndex
			out.AlreadyComplete = true
			return out, nil
		}
	}

	// --- transform stage -------------------------------------------------
	enc := transform.NewEncoder()
	startIdx := 0
	cur := &commitCursor{}
	if cfg.Resume {
		prev, ok, err := cfg.Store.Load()
		if err != nil {
			return out, err
		}
		if ok && !prev.Done {
			if prev.SourceSHA256 != sourceHash {
				return out, fault.New(fault.Checkpoint, 0, "checkpoint belongs to a different source")
			}
			prefix, err := cfg.Store.ReadStaging()
			if err != nil {
				return out, err
			}
			if int64(len(prefix)) != prev.EmittedBytes {
				return out, fault.New(fault.Checkpoint, 0,
					"staged prefix is %d bytes, checkpoint expects %d", len(prefix), prev.EmittedBytes)
			}
			if checkpoint.PrefixDigest(prefix) != prev.PrefixSHA256 {
				return out, fault.New(fault.Checkpoint, 0, "staged prefix digest mismatch")
			}
			if err := enc.Restore(prefix); err != nil {
				return out, err
			}
			startIdx = prev.NextIndex
			if startIdx > len(scan.Records) {
				return out, fault.New(fault.Checkpoint, 0,
					"checkpoint cursor %d exceeds %d scanned records", startIdx, len(scan.Records))
			}
			out.Resumed = true
			cur.staged = len(prefix)
		}
	}
	// A resumed run measures everything after the checkpoint is restored; the
	// start marker is taken when resume was requested so checkpoint loading
	// is included. A fresh run keeps ResumeMillis at zero.
	if !cfg.Resume {
		resumeClock = time.Time{}
	}

	for i := startIdx; i < len(scan.Records); i++ {
		rec := scan.Records[i]
		if ctx.Err() != nil {
			if err := commit(cfg.Store, enc, sourceHash, i, out, cur); err != nil {
				return out, err
			}
			out.NextIndex = i
			out.Kept, out.Dropped = enc.Kept(), droppedCount(scan, enc.Kept())
			return finish(out), fault.Wrap(fault.Canceled, rec.Offset, ctx.Err(),
				"canceled after %d records", i)
		}
		if _, hit := transform.Overlaps(rec, ranges); hit {
			enc.Drop(rec.Offset)
		} else {
			enc.Add(rec)
		}
		out.NextIndex = i + 1
		out.Kept, out.Dropped = enc.Kept(), droppedCount(scan, enc.Kept())

		if cfg.InterruptAfter > 0 && i+1 == cfg.InterruptAfter && i+1 < len(scan.Records) {
			if err := commit(cfg.Store, enc, sourceHash, i+1, out, cur); err != nil {
				return out, err
			}
			out.Interrupted = true
			out.ResumeMillis = sinceMillis(resumeClock)
			return out, ErrInterrupted
		}
		if (i+1-startIdx)%batch == 0 && i+1 < len(scan.Records) {
			if err := commit(cfg.Store, enc, sourceHash, i+1, out, cur); err != nil {
				return out, err
			}
		}
	}

	// --- finalise --------------------------------------------------------
	final, err := enc.Finalize(uint8(scan.Header.Tracks))
	if err != nil {
		return out, err
	}
	// Validate the complete output before anything is marked successful.
	if _, err := wire.Scan(final); err != nil {
		return out, fault.Wrap(fault.Verify, 0, err, "final output failed validation")
	}
	digest := verify.SHA256Hex(final)
	if err := cfg.Store.WriteOutput(final); err != nil {
		return out, err
	}
	// Persist the remaining staged bytes and the final cursor, then mark the
	// run done in a second atomic write so a crash between them leaves a
	// resumable, not-done checkpoint.
	stream := enc.Bytes()
	if err := cfg.Store.CommitAppend(checkpoint.State{
		SourceSHA256: sourceHash,
		NextIndex:    len(scan.Records),
		Kept:         enc.Kept(),
		Dropped:      droppedCount(scan, enc.Kept()),
		EmittedBytes: int64(len(stream)),
		PrefixSHA256: enc.Digest(),
	}, stream, stream[cur.staged:]); err != nil {
		return out, err
	}
	if err := cfg.Store.Save(checkpoint.State{
		SourceSHA256: sourceHash,
		NextIndex:    len(scan.Records),
		Kept:         enc.Kept(),
		Dropped:      droppedCount(scan, enc.Kept()),
		EmittedBytes: int64(len(stream)),
		PrefixSHA256: enc.Digest(),
		Done:         true,
		OutputSHA256: digest,
	}); err != nil {
		return out, err
	}
	out.Output = final
	out.OutputSHA256 = digest
	out.Kept = enc.Kept()
	out.Dropped = droppedCount(scan, enc.Kept())
	out.NextIndex = len(scan.Records)
	if out.Resumed {
		out.ResumeMillis = sinceMillis(resumeClock)
	}
	return finish(out), nil
}
