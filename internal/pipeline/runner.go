package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"relaycheck/internal/clock"
	"relaycheck/internal/demux"
	"relaycheck/internal/mapping"
	"relaycheck/internal/provenance"
	"relaycheck/internal/recovery"
)

// Run executes the relay pipeline and returns the run report. A planned
// stop returns ErrInterrupted; unrecoverable input returns an error.
func Run(cfg Config) (*Report, error) {
	cfg.defaults()
	initPath, files, err := listInputs(cfg.Input)
	if err != nil {
		return nil, err
	}
	initData, err := os.ReadFile(initPath)
	if err != nil {
		return nil, err
	}
	initInfo, fault := demux.ParseInit(initData)
	if fault != nil {
		return nil, fmt.Errorf("unrecoverable init: %s", fault)
	}
	mapper, mapFaults := mapping.NewMapper(initInfo)
	digest, err := inputDigest(cfg.Input, files)
	if err != nil {
		return nil, err
	}
	params := recovery.ParamsDigest(digest, cfg.Chunk)
	idx, err := recovery.Open(cfg.IndexPath)
	if err != nil {
		return nil, err
	}
	defer idx.Close()
	if idx.Header == nil {
		if err := idx.Append(&recovery.Record{Type: "header", Version: recovery.IndexVersion,
			ParamsDigest: params, InputDigest: digest, ChunkSize: cfg.Chunk}); err != nil {
			return nil, err
		}
	} else if idx.Header.ParamsDigest != params || idx.Header.Version != recovery.IndexVersion {
		return nil, fmt.Errorf("index does not match input digest or parameters; refusing to resume")
	}
	if err := os.MkdirAll(cfg.Output, 0o755); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(cfg.Output, "init.mp4"), initData); err != nil {
		return nil, err
	}
	trace, err := provenance.Open(filepath.Join(cfg.Output, "provenance"))
	if err != nil {
		return nil, err
	}
	byPos := map[uint32][]inputFile{}
	var maxSeq uint32
	for _, f := range files {
		byPos[f.pos] = append(byPos[f.pos], f)
		if f.pos > maxSeq {
			maxSeq = f.pos
		}
	}
	chunkCount := int((maxSeq + uint32(cfg.Chunk) - 1) / uint32(cfg.Chunk))
	if idx.Done != nil {
		// idempotent rerun: everything is committed; rebuild the report
		traces, err := trace.Load()
		if err != nil {
			return nil, err
		}
		rep := BuildReport(cfg, idx, mapper, traces, chunkCount, mapFaults)
		return rep, writeReport(cfg.Output, rep)
	}
	r := &runner{cfg: cfg, mapper: mapper, lastDur: map[uint32]int64{}, timelines: map[uint32]*clock.Timeline{}}
	for _, t := range mapper.Tracks() {
		r.timelines[t.OutID] = clock.NewTimeline(t.Info.Timescale)
	}
	r.restore(idx)
	cp := recovery.NewCheckpointer(idx)
	committed := 0
	for c := 1; c <= chunkCount; c++ {
		if cp.Committed(c) {
			continue
		}
		first := uint32(c-1)*uint32(cfg.Chunk) + 1
		last := first + uint32(cfg.Chunk) - 1
		if last > maxSeq {
			last = maxSeq
		}
		if err := r.processChunk(c, first, last, byPos, trace, cp); err != nil {
			return nil, err
		}
		committed++
		if cfg.StopAfter > 0 && committed >= cfg.StopAfter {
			return nil, ErrInterrupted
		}
	}
	traces, err := trace.Load()
	if err != nil {
		return nil, err
	}
	rep := BuildReport(cfg, idx, mapper, traces, chunkCount, mapFaults)
	if len(r.unrecoverable) > 0 {
		writeReport(cfg.Output, rep)
		return rep, fmt.Errorf("unrecoverable input: %v", r.unrecoverable)
	}
	if err := cp.Finish(rep.ResultsDigest); err != nil {
		return nil, err
	}
	if err := writeReport(cfg.Output, rep); err != nil {
		return nil, err
	}
	return rep, nil
}

func (r *runner) processChunk(chunk int, first, last uint32, byPos map[uint32][]inputFile,
	trace *provenance.Trace, cp *recovery.Checkpointer) error {
	var files []inputFile
	for pos := first; pos <= last; pos++ {
		files = append(files, byPos[pos]...)
	}
	p := newParser(r.cfg.Input, r.cfg.Workers, newMemBudget(int64(r.cfg.MaxMemMB)<<20))
	p.submit(files)
	pending := map[uint32][]*parsedFile{}
	var traces []provenance.SegmentTrace
	accepted := 0
	h := sha256.New()
	record := func(tr provenance.SegmentTrace) {
		traces = append(traces, tr)
		fmt.Fprintf(h, "%d:%s;", tr.Seq, tr.Decision)
		for _, tt := range tr.Tracks {
			fmt.Fprintf(h, "%d:%s;", tt.TrackID, tt.OutputSHA)
		}
		if tr.Decision == provenance.Accepted {
			accepted++
		}
	}
	for pos := first; pos <= last; pos++ {
		fl := byPos[pos]
		if len(fl) == 0 {
			f := clock.NewFault(clock.FaultLoss, pos, 0, -1, "segment file missing")
			tr := provenance.SegmentTrace{Seq: pos, Decision: provenance.Lost, Repairs: []string{f.String()}}
			r.skipTimelines(pos, clock.FaultLoss, &tr)
			record(tr)
			continue
		}
		var prs []*parsedFile
		for prs == nil {
			pr, ok := <-p.res
			if !ok {
				return fmt.Errorf("parser closed before position %d", pos)
			}
			pending[pr.file.pos] = append(pending[pr.file.pos], pr)
			prs = pending[pos]
		}
		delete(pending, pos)
		trs, err := r.processPosition(pos, prs)
		for _, pr := range prs {
			p.release(pr)
		}
		if err != nil {
			return err
		}
		for _, tr := range trs {
			record(tr)
		}
	}
	if err := trace.WriteChunk(chunk, traces); err != nil {
		return err
	}
	next, dur := r.stateMaps()
	return cp.Commit(recovery.Record{
		Chunk: chunk, FirstSeq: first, LastSeq: last,
		OutDigest: hex.EncodeToString(h.Sum(nil)), Segments: accepted,
		Next: next, Dur: dur,
	})
}
