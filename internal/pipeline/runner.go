package pipeline

import (
	"encoding/json"
	"io"
	"path/filepath"

	"mediarun/internal/ingest"
	"mediarun/internal/mapping"
	"mediarun/internal/mediaerr"
	"mediarun/internal/remux"
)

// Run executes one full ingest -> resolve/map -> remux job with back-pressure,
// checkpointing, provenance and statistics. On any structured error the output
// is aborted (never committed) and the error is returned for exit-code mapping.
func Run(opts Options) (*Stats, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	depth := opts.QueueDepth
	if depth <= 0 {
		depth = DefaultQueueDepth
	}

	inputHash, err := HashFile(opts.InputPath)
	if err != nil {
		return nil, err
	}
	cfgRaw, err := json.Marshal(opts.Mapping)
	if err != nil {
		return nil, mediaerr.New(mediaerr.CodeInvalidConfig, err.Error())
	}
	mappingHash, err := HashConfig(cfgRaw)
	if err != nil {
		return nil, err
	}

	streamer, err := ingest.Open(opts.InputPath)
	if err != nil {
		return nil, err
	}
	defer streamer.Close()

	plan, err := mapping.Build(opts.Mapping, streamer.Info().Tracks)
	if err != nil {
		return nil, err
	}
	decisions, err := resolveAll(opts, streamer.Info())
	if err != nil {
		return nil, err
	}

	resumeFrom, _, err := prepareResume(opts, inputHash, mappingHash)
	if err != nil {
		return nil, err
	}

	outputPath := filepath.Join(opts.WorkDir, opts.OutputName)
	var writer *remux.Writer
	if resumeFrom > 0 {
		writer, _, err = remux.NewAppendingWriter(outputPath, plan)
	} else {
		writer, err = remux.NewWriter(outputPath, plan)
	}
	if err != nil {
		return nil, err
	}

	stats := &Stats{
		TargetBitrateBPS: opts.TargetBitrate,
		InputSHA256:      inputHash,
		MappingSHA256:    mappingHash,
		Format:           streamer.Info().Format,
		ResumeFromSample: resumeFrom,
		ticks:            map[string]int64{},
		bytes:            map[string]int64{},
		scale:            map[string]int64{},
	}
	for _, tr := range streamer.Info().Tracks {
		stats.scale[tr.ID] = tr.Timescale
	}
	trace := NewTrace(inputHash, mappingHash, mappingRules(plan), decisions)

	if err := streamAndRemux(opts, streamer, plan, writer, resumeFrom, int64(depth), stats, trace); err != nil {
		if mediaerr.Is(err, mediaerr.CodeIO) && isInterrupt(err) {
			// A clean interrupt is a valid partial-commit boundary: finalize
			// the staged output atomically so --resume can append to it.
			hash, cerr := writer.Commit()
			if cerr != nil {
				return stats, cerr
			}
			stats.OutputSHA256 = hash
			return checkpointInterrupt(opts, stats, outputPath, trace)
		}
		writer.Abort()
		return stats, err
	}

	hash, err := writer.Commit()
	if err != nil {
		return stats, err
	}
	stats.OutputSHA256 = hash
	stats.OutputCommitted = true

	if err := finalizeTraceAndCheckpoint(opts, inputHash, mappingHash, hash, stats, trace); err != nil {
		return stats, err
	}
	fillBitrateAndAlignment(stats, streamer.Info())
	return stats, nil
}

// streamAndRemux runs bounded producer (ingest) and consumer (remux)
// concurrently. A full queue blocks the parser, so slow writes apply
// back-pressure. Resume skips already-committed samples by global index.
func streamAndRemux(opts Options, s ingest.Streamer, plan *mapping.Plan,
	w *remux.Writer, resumeFrom, depth int64, stats *Stats, trace *Trace) error {
	q := NewBoundedQueue(int(depth))
	accepted := map[string]bool{}
	for _, t := range plan.Targets {
		accepted[t.Source.ID] = true
	}

	producerErr := make(chan error, 1)
	go func() {
		cb := newChunkBlock()
		for {
			ev, err := s.Next()
			if err == io.EOF {
				if len(cb.current.Samples) > 0 {
					if perr := q.Push(cb.current); perr != nil {
						producerErr <- perr
						return
					}
				}
				q.Close()
				producerErr <- nil
				return
			}
			if err != nil {
				producerErr <- err
				q.Close()
				return
			}
			stats.SamplesIn++
			if !accepted[ev.TrackID] {
				stats.DroppedSamples++
				continue
			}
			if cb.add(ev) {
				if perr := q.Push(cb.current); perr != nil {
					producerErr <- perr
					return
				}
				cb.reset()
			}
		}
	}()

	var consumed int64
	for blk := range q.ch {
		for _, ev := range blk.Samples {
			if consumed < resumeFrom {
				// Already committed in the prior run: count once, skip the
				// rewrite, but still include QC, timing and bytes in full-run
				// accounting so statistics describe the whole deliverable.
				if err := qualityCheck(ev); err != nil {
					return mediaerr.New(mediaerr.CodeIO,
						"resumed sample failed quality check: "+err.Error()).
						At("", ev.TrackID, ev.DataOffset)
				}
				stats.QualityChecked++
				stats.ticks[ev.TrackID] += ev.Duration
				stats.bytes[ev.TrackID] += ev.Size
				consumed++
				stats.SamplesOut++
				continue
			}
			if err := qualityCheck(ev); err != nil {
				stats.QualityErrors++
				return mediaerr.New(mediaerr.CodeIO, "quality check failed: "+err.Error()).
					At("", ev.TrackID, ev.DataOffset)
			}
			stats.QualityChecked++
			stats.ticks[ev.TrackID] += ev.Duration
			stats.bytes[ev.TrackID] += ev.Size
			span := trace.NextOutputSpan(ev.TrackID, ev.Size)
			if err := w.WriteSample(ev); err != nil {
				return err
			}
			trace.Record(TraceEntry{
				TrackID: ev.TrackID, SampleIdx: ev.Index,
				InputSpan:  Span{Start: ev.DataOffset, End: ev.DataOffset + ev.Size},
				OutputSpan: span, PTS: ev.PTS, DTS: ev.DTS,
			})
			consumed++
			stats.SamplesOut++
			if opts.InterruptAfter > 0 && stats.SamplesOut >= opts.InterruptAfter {
				return errInterrupt
			}
		}
	}
	return <-producerErr
}
