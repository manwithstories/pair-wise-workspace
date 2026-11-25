// Package pipeline orchestrates the recovery flow: demux, keyframe indexing,
// content-addressed caching, checkpointing and lossless remux. It owns no
// parsing or writing logic itself; those stay in the focused subsystems.
package pipeline

import (
	"path/filepath"

	"archivecheck/internal/archive"
	"archivecheck/internal/demux"
	"archivecheck/internal/index"
	"archivecheck/internal/media"
	"archivecheck/internal/remux"
)

// SegmentResult summarizes how one segment was handled in a run.
type SegmentResult struct {
	Segment     string   `json:"segment"`
	Status      string   `json:"status"` // recovered | quarantined | cache_hit
	SourceSHA   string   `json:"source_sha256"`
	FragmentSHA string   `json:"fragment_sha256,omitempty"`
	Output      string   `json:"output,omitempty"`
	Samples     int      `json:"samples"`
	Tracks      int      `json:"tracks"`
	Keyframes   int      `json:"keyframes"`
	Rebuilt     bool     `json:"rebuilt_index"`
	CacheHit    bool     `json:"cache_hit"`
	Faults      []*media.Fault `json:"faults,omitempty"`
}

// Stats aggregates a whole batch for the terminal JSON report.
type Stats struct {
	Total       int `json:"total"`
	Recovered   int `json:"recovered"`
	Quarantined int `json:"quarantined"`
	CacheHits   int `json:"cache_hits"`
	Rebuilt     int `json:"rebuilt_indexes"`
	Duplicates  int `json:"duplicates"` // always 0: checkpoint prevents re-ingest
	RoundtripOK int `json:"roundtrip_ok"`
}

// RunResult is the full outcome of a batch.
type RunResult struct {
	Batch    string          `json:"batch"`
	Stats    Stats           `json:"stats"`
	Segments []SegmentResult `json:"segments"`
	Faults   []*media.Fault  `json:"faults"`
}

// BatchItem is one unit of work: a source file and its manifest identity.
type BatchItem struct {
	ID         string
	SourcePath string
}

// Runner holds the shared stores for a batch.
type Runner struct {
	Root           string
	Batch          string
	Cache          *archive.Cache
	Checkpt        *archive.Checkpoint
	ParseCfg       demux.ParseConfig
	ForceRecompute map[string]bool
}

// NewRunner wires the cache and checkpoint for a batch under root.
func NewRunner(root, batch string) (*Runner, error) {
	cache, err := archive.NewCache(root)
	if err != nil {
		return nil, err
	}
	cp, err := archive.OpenCheckpoint(root, batch, demux.FormatVersion)
	if err != nil {
		return nil, err
	}
	return &Runner{
		Root: root, Batch: batch, Cache: cache, Checkpt: cp,
		ParseCfg:       demux.DefaultParseConfig(),
		ForceRecompute: map[string]bool{},
	}, nil
}

// ForceRecomputeSegment drops one segment's checkpoint and cache entry so the
// next run recomputes it. It models a forged/tampered cache being detected and
// rejected rather than blindly trusted.
func (r *Runner) ForceRecomputeSegment(segment string) error {
	if st := r.Checkpt.Get(segment); st != nil && st.CacheKey != "" {
		if err := r.Cache.Invalidate(st.CacheKey); err != nil {
			return err
		}
	}
	return r.Checkpt.Rollback(segment)
}

// Run processes every item, isolating failures so one bad segment never blocks
// the others. On resume, recovered segments are skipped (no duplicates) and
// quarantined segments remain quarantined.
func (r *Runner) Run(items []BatchItem) (*RunResult, error) {
	res := &RunResult{Batch: r.Batch, Stats: Stats{Total: len(items)}}
	var allFaults media.FaultSet

	for _, item := range items {
		sr := r.process(item, &allFaults)
		res.Segments = append(res.Segments, sr)
		switch sr.Status {
		case "cache_hit":
			res.Stats.CacheHits++
			res.Stats.Recovered++
			res.Stats.RoundtripOK++
		case "recovered":
			res.Stats.Recovered++
			res.Stats.RoundtripOK++
			if sr.Rebuilt {
				res.Stats.Rebuilt++
			}
		case "quarantined":
			res.Stats.Quarantined++
		}
	}
	res.Faults = allFaults.All()
	return res, nil
}

func (r *Runner) process(item BatchItem, allFaults *media.FaultSet) SegmentResult {
	sourceSHA, err := archive.DigestSource(item.SourcePath)
	if err != nil {
		return r.quarantine(item, sourceSHA, allFaults, err)
	}

	// Resume: a fully recovered, unchanged segment is served from its record
	// without touching the demuxer, so re-runs cannot duplicate samples. A
	// segment flagged for forced recompute (forged cache) bypasses this path.
	if !r.ForceRecompute[item.ID] && r.Checkpt.IsDone(item.ID, sourceSHA) {
		st := r.Checkpt.Get(item.ID)
		return SegmentResult{
			Segment: item.ID, Status: "cache_hit", SourceSHA: sourceSHA,
			FragmentSHA: st.FragmentSHA, Output: st.OutputPath,
			Samples: st.Samples, Tracks: st.Tracks, CacheHit: true,
		}
	}
	if r.Checkpt.IsQuarantined(item.ID) {
		st := r.Checkpt.Get(item.ID)
		sr := SegmentResult{Segment: item.ID, Status: "quarantined", SourceSHA: sourceSHA, Faults: st.Faults}
		for _, f := range st.Faults {
			allFaults.Add(f)
		}
		return sr
	}

	tracks, report, derr := demux.ParseSource(item.SourcePath, item.ID, r.ParseCfg)
	var localFaults media.FaultSet
	for _, f := range report.Faults {
		localFaults.Add(f)
	}
	if derr != nil {
		// A fatal demux fault means the source package is not wholly trustworthy
		// (corrupt length, unsupported codec, bad container). The segment is
		// quarantined with its located fault; non-fatal truncations return a
		// nil error and still recover their complete samples.
		return r.quarantine(item, sourceSHA, allFaults, derr)
	}

	// Keyframe indexing with bounded rebuild.
	totalKeyframes, rebuilt := r.indexTracks(tracks, &localFaults)

	// Build remux inputs from whatever tracks survived.
	inputs := make([]remux.TrackInput, 0, len(tracks))
	totalSamples := 0
	for _, tr := range tracks {
		if len(tr.Samples) == 0 {
			continue
		}
		inputs = append(inputs, remux.TrackInput{Meta: tr.Meta, Samples: tr.Samples})
		totalSamples += len(tr.Samples)
	}
	if len(inputs) == 0 {
		return r.quarantine(item, sourceSHA, allFaults,
			media.NewFault(media.KindBadLength, item.ID, 0, -1, "no recoverable samples"))
	}

	out := filepath.Join(r.Root, "fragments", item.ID+".mp4")
	written, werr := remux.WriteFragment(out, inputs)
	if werr != nil {
		return r.quarantine(item, sourceSHA, allFaults, werr)
	}

	// Cache key binds source digest, mapping, boundaries and format version.
	mapping := canonicalMapping(tracks)
	key := archive.Key(archive.CacheKeyInput{
		SourceSHA256: sourceSHA, Mapping: mapping, Format: demux.FormatVersion,
	})
	fragSHA := hashToHex(written.SHA256)
	entry := archive.CacheEntry{
		Key: key, FragmentPath: out, FragmentSHA: fragSHA,
		SourceSHA: sourceSHA, Format: demux.FormatVersion, Samples: totalSamples,
	}
	if err := r.Cache.Put(entry); err != nil {
		return r.quarantine(item, sourceSHA, allFaults, err)
	}

	// Checkpoint only after output landed and digest was computed.
	if err := r.Checkpt.CommitRecovered(archive.SegmentState{
		Segment: item.ID, OutputPath: out, FragmentSHA: fragSHA, SourceSHA: sourceSHA,
		CacheKey: key, Samples: totalSamples, Tracks: len(inputs), Faults: localFaults.All(),
	}); err != nil {
		return r.quarantine(item, sourceSHA, allFaults, err)
	}

	for _, f := range localFaults.All() {
		allFaults.Add(f)
	}
	return SegmentResult{
		Segment: item.ID, Status: "recovered", SourceSHA: sourceSHA,
		FragmentSHA: fragSHA, Output: out, Samples: totalSamples,
		Tracks: len(inputs), Keyframes: totalKeyframes, Rebuilt: rebuilt,
		Faults: localFaults.All(),
	}
}

// indexTracks computes authoritative keyframe indexes, returning total
// keyframes and whether any track required a bounded scan rebuild.
func (r *Runner) indexTracks(tracks []demux.TrackSamples, faults *media.FaultSet) (keyframes int, rebuilt bool) {
	for i := range tracks {
		if tracks[i].Meta.Handler != "vide" {
			continue
		}
		kf, rep := index.IndexFor(tracks[i], index.ScanBudget)
		keyframes += len(kf)
		if rep.Rebuilt {
			rebuilt = true
		}
		for _, f := range rep.Faults {
			faults.Add(f)
		}
	}
	return keyframes, rebuilt
}

func (r *Runner) quarantine(item BatchItem, sourceSHA string, all *media.FaultSet, err error) SegmentResult {
	faults := media.FaultSet{}
	faults.Add(err)
	_ = r.Checkpt.CommitQuarantined(archive.SegmentState{
		Segment: item.ID, SourceSHA: sourceSHA, Faults: faults.All(),
	})
	for _, f := range faults.All() {
		all.Add(f)
	}
	return SegmentResult{
		Segment: item.ID, Status: "quarantined", SourceSHA: sourceSHA, Faults: faults.All(),
	}
}
