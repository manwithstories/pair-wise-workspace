package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"mediascan/internal/analyze"
	"mediascan/internal/index"
	"mediascan/internal/manifest"
	"mediascan/internal/model"
	"mediascan/internal/recover"
	"mediascan/internal/report"
	"mediascan/internal/stream"
)

// Run executes scan, stream analysis, RTP analysis and provenance writing.
//
// When interruptAfter > 0 the scan deliberately stops after that many fully
// hashed segments, persists an interrupted index and checkpoint, and returns
// ErrInterrupted together with the partial result. A later Resume call
// continues from that position.
func Run(o Options, interruptAfter int) (*Result, error) {
	o.Defaults()
	res, primary, secondary, err := Load(o)
	if err != nil {
		return nil, err
	}
	params, err := o.Params().Normalize()
	if err != nil {
		return nil, err
	}
	paths := index.NewPaths(o.Work)
	prev, err := index.ReadIndex(paths.Index)
	if err != nil {
		prev = nil // a missing or unreadable prior index simply means no resume
	}
	store := recover.NewStore(o.Work)
	prevCP, err := store.Load()
	if err != nil {
		prevCP = nil // a corrupt checkpoint is treated as absent, never trusted
	}
	resumeFrom := 0
	if prevCP != nil && prevCP.Validate(recover.BindingOf(prev, params)) == nil {
		resumeFrom = len(recover.CompletedEntries(prevCP, prev))
	}
	idx, scanErr := index.Scan(res, primary, index.Options{
		ResumeFrom: resumeFrom, Prev: prev, StopAfter: interruptAfter,
	})
	if scanErr != nil && scanErr != index.ErrInterrupted {
		return nil, scanErr
	}
	cp := recover.CheckpointForIndex(idx, params, prevCP)
	if scanErr == index.ErrInterrupted {
		recover.SimulateInterrupt(cp, len(idx.Entries))
		cp.Completed = idx.Scanned + idx.Skipped
		cp.NextSeg = len(idx.Entries)
	} else {
		cp.Completed = idx.Scanned + idx.Skipped
		cp.NextSeg = len(idx.Entries)
		cp.Status = "complete"
	}
	if err := store.Save(cp); err != nil {
		return nil, err
	}
	if err := index.WriteJSON(paths.Index, idx); err != nil {
		return nil, err
	}
	result, err := BuildResult(res, primary, secondary, idx, o)
	if err != nil {
		return nil, err
	}
	result.Scanned = idx.Scanned + idx.Skipped
	result.Checkpoint = cp.NextSeg
	result.ResumeReused = idx.Skipped
	result.Interrupted = scanErr == index.ErrInterrupted
	result.P95LatencyMS = percentile95(idx.Durations)
	resultSHA := result.ResultSHA
	cp.ResultSHA = resultSHA
	if err := store.Save(cp); err != nil {
		return nil, err
	}
	prov, err := report.New(o.Work)
	if err != nil {
		return nil, err
	}
	prov.Now = time.Now
	prov.Bind(idx, cp, resultSHA)
	if err := prov.Write(report.Record{Kind: "run",
		Detail: fmt.Sprintf("manifest=%s segments=%d interrupted=%v", primary.Source, result.Segments, result.Interrupted)}); err != nil {
		return nil, err
	}
	if err := prov.WriteFaults(result.Faults); err != nil {
		return nil, err
	}
	if scanErr == index.ErrInterrupted {
		return result, ErrInterrupted
	}
	return result, nil
}

// ErrInterrupted reports that the run stopped early and should be resumed.
var ErrInterrupted = fmt.Errorf("pipeline: run interrupted; resume to continue")

// resultDigestView fixes the digest input so adding latency or provenance
// fields later cannot silently change reproducibility semantics.
type resultDigestView struct {
	result *Result
}

// MarshalJSON renders the digest-relevant subset in a fixed field order.
func (v resultDigestView) MarshalJSON() ([]byte, error) {
	type view struct {
		InputSHA     string        `json:"input_sha256"`
		Manifest     string        `json:"manifest"`
		Segments     int           `json:"segments"`
		Missing      []string      `json:"missing,omitempty"`
		FirstMiss    string        `json:"first_missing,omitempty"`
		Corrupt      []model.Fault `json:"corrupt_ranges,omitempty"`
		AVMaxMS      int64         `json:"av_sync_max_ms"`
		AVMaxSeg     string        `json:"av_sync_max_segment"`
		AVJumps      int           `json:"av_real_jumps"`
		GOPMax       int64         `json:"max_gop_ms"`
		RTPDropped   int           `json:"packets_lost"`
		RTPDup       int           `json:"duplicates"`
		RTPRecovered int           `json:"recovered"`
		Faults       []model.Fault `json:"faults"`
	}
	return json.Marshal(view{
		InputSHA: v.result.InputSHA, Manifest: v.result.Manifest, Segments: v.result.Segments,
		Missing: v.result.Missing, FirstMiss: v.result.FirstMiss, Corrupt: v.result.CorruptRange,
		AVMaxMS: v.result.AVSync.MaxErrorMS, AVMaxSeg: v.result.AVSync.MaxErrorSeg,
		AVJumps: v.result.AVSync.RealJumps, GOPMax: v.result.Keyframes.MaxIntervalMS,
		RTPDropped: v.result.RTP.Dropped, RTPDup: v.result.RTP.Duplicates,
		RTPRecovered: v.result.RTP.Recovered, Faults: v.result.Faults,
	})
}

// timelineSegments counts every segment the primary manifest references,
// including slots whose media is missing. This keeps the reported total stable
// even when a run is interrupted early and the index is only partial.
func timelineSegments(tl *manifest.Timeline) int {
	n := 0
	for _, st := range tl.Streams {
		n += len(st.Segments)
	}
	return n
}

// dedupedFaults merges index and manifest faults. HLS and DASH commonly name
// the same missing media, so a fault is reported once per (kind, stream,
// segment) triple; byte ranges still distinguish distinct corruptions.
func dedupedFaults(idx *index.Index, primary, secondary *manifest.Timeline) model.Faults {
	seen := map[string]bool{}
	var out model.Faults
	add := func(f model.Fault) {
		key := fmt.Sprintf("%s|%s|%s|%d|%d", f.Kind, f.Stream, f.Segment, f.ByteStart, f.ByteEnd)
		if seen[key] {
			return
		}
		seen[key] = true
		out.Add(f)
	}
	for _, f := range idx.Faults {
		add(f)
	}
	for _, f := range manifestFaults(primary) {
		add(f)
	}
	if secondary != nil {
		for _, f := range manifestFaults(secondary) {
			add(f)
		}
	}
	return out
}

// BuildResult runs the analysis phases over an index and produces the
// digestible result. It writes nothing, so verify can reuse it unchanged.
func BuildResult(res *manifest.Resolver, primary, secondary *manifest.Timeline,
	idx *index.Index, o Options) (*Result, error) {
	o.Defaults()
	result := &Result{
		InputSHA:     idx.InputSHA,
		Manifest:     primary.Source,
		Format:       primary.Format,
		Segments:     timelineSegments(primary),
		Missing:      idx.Missing,
		FirstMiss:    idx.FirstMiss,
		IndexVersion: idx.Version,
	}
	faults := dedupedFaults(idx, primary, secondary)
	sr, err := AnalyzeSegments(res, idx, o)
	if err != nil {
		return nil, err
	}
	result.AVSync = sr.Sync
	result.Keyframes = sr.Keyframes
	result.CorruptRange = sr.Corrupt
	for _, f := range sr.Faults {
		faults.Add(f)
	}
	rtpStats, err := AnalyzeRTPFiles(o, idx.Entries)
	if err != nil {
		return nil, err
	}
	result.RTP = rtpStats
	for _, f := range rtpStats.Faults {
		faults.Add(f)
	}
	faults.Sort()
	result.Faults = faults.All()
	sha, err := recover.ResultDigest(resultDigestView{result})
	if err != nil {
		return nil, err
	}
	result.ResultSHA = sha
	return result, nil
}

func percentile95(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	i := int(float64(len(cp))*0.95 + 0.5)
	if i >= len(cp) {
		i = len(cp) - 1
	}
	return cp[i]
}

// AnalyzeSegments validates each present indexed segment and runs A/V and
// keyframe analysis over the surviving tracks.
func AnalyzeSegments(res *manifest.Resolver, idx *index.Index, o Options) (*StreamResult, error) {
	sr := &StreamResult{Segments: len(idx.Entries)}
	for _, entry := range idx.Entries {
		if !entry.Present {
			continue
		}
		full, err := res.Resolve(entry.URI)
		if err != nil {
			return nil, err
		}
		scan, err := stream.ScanSegment(nil, entry.Name, full, entry.ByteStart, entry.ByteEnd)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		f, bad := scan.FirstUndecodable()
		if !bad {
			continue
		}
		f.TimeStart, f.TimeEnd = entry.Start, mustEnd(entry)
		f.IndexEntry = entry.Ordinal
		sr.Corrupt = append(sr.Corrupt, f)
		sr.Faults = append(sr.Faults, f)
		if sr.FirstBad == "" {
			sr.FirstBad = entry.Name
		}
	}
	initSeg, err := loadInits(res)
	if err != nil {
		return sr, err
	}
	tracks, err := analyze.BuildTracks(idx, res, initSeg)
	if err != nil {
		return sr, err
	}
	var video, audio *analyze.Track
	for i := range tracks {
		switch tracks[i].Kind {
		case "video":
			video = &tracks[i]
		case "audio":
			audio = &tracks[i]
		}
	}
	if video != nil && audio != nil {
		sync, err := analyze.Align(*video, *audio, o.AVTolMS)
		if err != nil {
			return nil, err
		}
		sr.Sync = sync
		sr.Faults = append(sr.Faults, sync.Faults...)
	}
	if video != nil {
		rep := analyze.Keyframes(*video, o.MaxGOPMS)
		sr.Keyframes = rep
		sr.Faults = append(sr.Faults, rep.Faults...)
	}
	return sr, nil
}

// loadInits merges the track tables of the video and audio init segments so a
// single Init can describe a muxed stream.
func loadInits(res *manifest.Resolver) (*stream.Init, error) {
	merged := &stream.Init{Tracks: map[uint32]stream.TrackMeta{}}
	for _, cand := range []string{"media/init-v.mp4", "media/init-a.mp4"} {
		full, err := res.Resolve(cand)
		if err != nil {
			continue
		}
		if _, statErr := os.Stat(full); statErr != nil {
			continue
		}
		parsed, err := stream.LoadInit(full)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", model.ErrBoxBounds, cand, err)
		}
		for id, meta := range parsed.Tracks {
			merged.Tracks[id] = meta
		}
	}
	if len(merged.Tracks) == 0 {
		return nil, fmt.Errorf("pipeline: no initialization segment found")
	}
	return merged, nil
}

func mustEnd(e index.Entry) model.Rational {
	end, err := e.End()
	if err != nil {
		return e.Start
	}
	return end
}
