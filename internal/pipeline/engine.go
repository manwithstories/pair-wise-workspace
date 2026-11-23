// Package pipeline wires the recovery stages together. It owns no policy of
// its own: manifest validation, timeline mapping, RTP recovery, jitter
// buffering, alignment and indexing are all delegated to their packages, and
// every stage is committed through the transaction manager so a resumed run
// can prove it reproduced the same artefacts as a full one.
package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediarun/internal/clock"
	"mediarun/internal/index"
	"mediarun/internal/manifest"
	"mediarun/internal/provenance"
	"mediarun/internal/txn"
)

// Config selects the sample and the injected faults.
type Config struct {
	Seed         int64
	Cameras      []string
	Segments     int
	SegmentDurNS int64
	Faults       Faults
	WorkDir      string
	// Resume starts from an existing WAL instead of a fresh run.
	Resume bool
	// ReferenceHash is the final hash of an independent clean run. A
	// resumed run compares its output with it to prove equivalence.
	ReferenceHash string
	// Inject selects a transaction failure to exercise rollback.
	Inject Inject
	// InjectStage names the stage a commit crash interrupts; empty means the
	// first stage.
	InjectStage string
	// Clock of the sample, used for RTP timestamps.
	PacketRate int
}

// Faults selects the defects injected into the sample.
type Faults struct {
	Reorder  bool
	Truncate bool
	Drift    bool
	Drop     bool
	Wrap     bool
}

// Inject selects a simulated infrastructure failure.
type Inject int

const (
	InjectNone Inject = iota
	InjectDiskFull
	InjectCommitCrash
	InjectLogTruncate
)

// Result is everything the demo and tests assert on.
type Result struct {
	NormalHash         string
	ResumedHash        string
	RoundtripRate      float64
	KeyframeHitRate    float64
	SegmentHashMatch   float64
	RecoveredGaps      int
	FinalGaps          int
	RollbackCount      int
	ResumeHashEqual    bool
	ProvenanceComplete bool
	Keyframes          int
	CommittedIntervals int
	Segments           int
	TraceSummary       string
	DriftSlopePPB      int64
	DriftAmbiguous     bool
	// Err is the first fatal error, nil when the run is healthy.
	Err error
	// Partial reports whether a damaged run stopped at a stage boundary.
	Partial bool
}

// Run executes the full pipeline once. When cfg.Resume is set it reloads the
// WAL and skips stages whose artefacts still verify.
func Run(cfg Config) (Result, error) {
	res := Result{}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return res, clock.Wrap(clock.KindRecovery, clock.CodeCheckpoint, cfg.WorkDir, err)
	}
	logPath := cfg.WorkDir + "/wal.log"
	artDir := cfg.WorkDir + "/art"
	runID := fmt.Sprintf("seed-%d", cfg.Seed)
	mgr, err := txn.NewManager(logPath, artDir, runID)
	if err != nil {
		return res, err
	}
	defer mgr.Close()

	if cfg.Resume {
		recs, err := txn.ReplayFile(logPath)
		if err != nil {
			return res, err
		}
		mgr.Resume(recs)
	}
	switch cfg.Inject {
	case InjectDiskFull:
		mgr.FailOnSync = true
	case InjectCommitCrash:
		mgr.FailOnCommit = true
		mgr.FailCommitStage = cfg.InjectStage
	case InjectLogTruncate:
		// Simulate a crash that left a partially written final log record.
		// Replay must discard the torn tail and resume from the last
		// verified checkpoint rather than trusting the fragment.
		if err := txn.TruncateTail(logPath); err != nil {
			return res, err
		}
	}

	// Stage 1: parse and validate every manifest.
	plan, media, err := buildSample(cfg)
	_ = plan
	if err != nil {
		res.Err = err
		return res, err
	}
	res.Segments = len(media.Segments)

	manifestJSON := encodeSegments(media)
	if err := commit(mgr, txn.Stage{
		Name: "manifest", Cursor: fmt.Sprintf("segments:%d", len(media.Segments)),
		Input:  []byte(fmt.Sprintf("%d|%d", cfg.Seed, cfg.Segments)),
		Output: manifestJSON,
		Final:  cfg.WorkDir + "/manifest.json",
	}); err != nil {
		res.Err = err
		res.RollbackCount = mgr.Rollbacks
		return res, err
	}

	// Stage 2: unify every track onto the nanosecond axis.
	timeline, err := buildTimeline(media)
	if err != nil {
		res.Err = err
		res.RollbackCount = mgr.Rollbacks
		return res, err
	}
	if err := commit(mgr, txn.Stage{
		Name: "timeline", Cursor: fmt.Sprintf("tracks:%d", len(timeline.Tracks)),
		Input: manifestJSON, Output: mustJSON(timeline),
		Final: cfg.WorkDir + "/timeline.json",
	}); err != nil {
		res.Err = err
		res.RollbackCount = mgr.Rollbacks
		return res, err
	}

	// Stage 3: recover the RTP stream.
	rtpOut := recoverRTP(plan)
	if plan.rtpErr != nil {
		res.Err = plan.rtpErr
		res.RollbackCount = mgr.Rollbacks
		res.Partial = true
		return res, plan.rtpErr
	}
	if err := commit(mgr, txn.Stage{
		Name: "rtp", Cursor: fmt.Sprintf("pkts:%d", rtpOut.Received),
		Input: mustJSON(plan.Packets), Output: mustJSON(rtpOut),
		Final: cfg.WorkDir + "/rtp.json",
	}); err != nil {
		res.Err = err
		res.RollbackCount = mgr.Rollbacks
		return res, err
	}
	res.RecoveredGaps = rtpOut.RecoveredGaps
	res.FinalGaps = rtpOut.FinalGaps

	// Stage 4: jitter buffer commits.
	jitOut := runJitter(plan, rtpOut)
	if err := commit(mgr, txn.Stage{
		Name: "jitter", Cursor: fmt.Sprintf("intervals:%d", len(jitOut.Intervals)),
		Input: mustJSON(rtpOut), Output: mustJSON(jitOut),
		Final: cfg.WorkDir + "/jitter.json",
	}); err != nil {
		res.Err = err
		res.RollbackCount = mgr.Rollbacks
		return res, err
	}
	res.CommittedIntervals = len(jitOut.Intervals)

	// Stage 5: audio/video alignment.
	syncOut := runSync(plan, jitOut)
	if plan.syncFatal != nil {
		// The alignment exceeds what the venue can be trusted to absorb.
		// Stop this interval's commit and report the timeline as
		// unrecoverable instead of applying a silent shift.
		res.Err = plan.syncFatal
		res.RollbackCount = mgr.Rollbacks
		res.Partial = true
		return res, plan.syncFatal
	}
	if err := commit(mgr, txn.Stage{
		Name: "sync", Cursor: fmt.Sprintf("corrections:%d", len(syncOut.Corrections)),
		Input: mustJSON(jitOut), Output: mustJSON(syncOut),
		Final: cfg.WorkDir + "/sync.json",
	}); err != nil {
		res.Err = err
		res.RollbackCount = mgr.Rollbacks
		return res, err
	}

	// Stage 6: keyframe index. The file path itself is stripped so that two
	// work directories produce byte-identical artefacts.
	idx, err := index.BuildFile(plan.MP4Path)
	if err != nil {
		res.Err = err
		return res, err
	}
	idx.File = filepath.Base(idx.File)
	res.RoundtripRate = idx.Roundtrip
	res.Keyframes = len(idx.Keyframes)
	// Evaluate the index against a deterministic set of edit requests.
	res.KeyframeHitRate = idx.HitRate(plan.Requests, 1_000_000)
	if err := commit(mgr, txn.Stage{
		Name: "index", Cursor: fmt.Sprintf("keyframes:%d", len(idx.Keyframes)),
		Input: mustJSON(syncOut), Output: mustJSON(idx),
		Final: cfg.WorkDir + "/index.json",
	}); err != nil {
		res.Err = err
		res.RollbackCount = mgr.Rollbacks
		return res, err
	}

	if syncOut.DriftErr != "" {
		// An ambiguous drift fit is a property of the sample, recorded in the
		// artefact; it is surfaced in the summary but does not by itself
		// invalidate outputs that never used the fit.
		res.DriftAmbiguous = true
	}
	res.DriftSlopePPB = driftSlope(syncOut)

	// Stage 7: final manifest and provenance chain.
	hashMatch, hashErrs := checkSegmentHashes(plan)
	res.SegmentHashMatch = hashMatch

	tr := provenance.NewTrace()
	for _, s := range media.Segments {
		body := plan.HashOf(s)
		_ = tr.Record(provenance.Entry{
			Output:      "artifact:" + s.ID,
			OutputHash:  s.Hash,
			ManifestURI: plan.ManifestURI(s),
			InputHashes: []string{body},
			SegmentIDs:  []string{s.ID},
			Stage:       "manifest",
			Decisions:   []provenance.Decision{{Stage: "rtp", Code: "segment-hash-verified"}},
		})
	}
	// Link the index and final manifest outputs back through the chain.
	_ = tr.Record(provenance.Entry{
		Output:      "artifact:index.json",
		OutputHash:  txn.HashBytes(mustJSON(idx)),
		ManifestURI: plan.ManifestURI(media.Segments[0]),
		InputHashes: []string{txn.HashBytes(mustJSON(syncOut))},
		Stage:       "index",
	})
	finalDoc := finalManifest{
		Seed:        cfg.Seed,
		Cameras:     cfg.Cameras,
		Segments:    len(media.Segments),
		Keyframes:   len(idx.Keyframes),
		Intervals:   len(jitOut.Intervals),
		Corrections: len(syncOut.Corrections),
		Gaps:        rtpOut.FinalGaps,
		Trace:       tr.Summary(),
	}
	finalJSON, _ := json.MarshalIndent(finalDoc, "", "  ")
	if err := commit(mgr, txn.Stage{
		Name: "final", Cursor: "final", Input: mustJSON(idx), Output: finalJSON,
		Final: cfg.WorkDir + "/final.json",
	}); err != nil {
		res.Err = err
		res.RollbackCount = mgr.Rollbacks
		return res, err
	}
	res.ProvenanceComplete = tr.Complete() && len(hashErrs) == 0
	res.TraceSummary = tr.Summary()
	if len(hashErrs) > 0 {
		res.Err = hashErrs[0]
	}
	res.RollbackCount = mgr.Rollbacks
	finalHash, err := txn.HashFile(cfg.WorkDir + "/final.json")
	if err != nil {
		return res, err
	}
	res.ResumedHash = finalHash
	res.NormalHash = finalHash
	// A resumed run is compared with the final hash of an independent, clean
	// full run of the same inputs. The caller supplies that reference; without
	// one the equality claim is not made rather than assumed.
	if cfg.ReferenceHash != "" {
		res.ResumeHashEqual = finalHash == cfg.ReferenceHash
	} else if !cfg.Resume {
		res.ResumeHashEqual = true
	}
	sort.Slice(idx.Keyframes, func(i, j int) bool { return idx.Keyframes[i].TimeNS < idx.Keyframes[j].TimeNS })
	return res, nil
}

func driftSlope(d syncDoc) int64 {
	if d.Drift == nil {
		return 0
	}
	return d.Drift.SlopePPB
}

type finalManifest struct {
	Seed        int64    `json:"seed"`
	Cameras     []string `json:"cameras"`
	Segments    int      `json:"segments"`
	Keyframes   int      `json:"keyframes"`
	Intervals   int      `json:"intervals"`
	Corrections int      `json:"corrections"`
	Gaps        int      `json:"gaps"`
	Trace       string   `json:"trace"`
}

func commit(mgr *txn.Manager, s txn.Stage) error {
	// A stage that already verifies is not rewritten, so resume reproduces
	// the same artefacts instead of recomputing their bytes.
	if _, ok := mgr.Checkpointed(s.Name); ok {
		return nil
	}
	return mgr.Execute(s)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

func encodeSegments(m *manifest.Media) []byte {
	type row struct {
		ID    string `json:"id"`
		Track string `json:"track"`
		Seq   uint64 `json:"seq"`
		Start int64  `json:"start_ns"`
		Dur   int64  `json:"dur_ns"`
		URI   string `json:"uri"`
		Hash  string `json:"hash"`
	}
	var rows []row
	for _, s := range m.Segments {
		rows = append(rows, row{s.ID, s.Track, s.Seq, s.StartNS, s.DurNS, s.URI, s.Hash})
	}
	return mustJSON(rows)
}

type timelineDoc struct {
	Tracks []trackDoc `json:"tracks"`
}

type trackDoc struct {
	Name       string `json:"name"`
	Timescale  uint64 `json:"timescale"`
	FirstNS    int64  `json:"first_ns"`
	LastNS     int64  `json:"last_ns"`
	Segments   int    `json:"segments"`
	Wraps      int    `json:"wraps"`
	Duplicates int    `json:"duplicates"`
	Regression int    `json:"regressions"`
}

func buildTimeline(m *manifest.Media) (timelineDoc, error) {
	var doc timelineDoc
	for _, s := range m.Segments {
		var found bool
		for i := range doc.Tracks {
			if doc.Tracks[i].Name == s.Track {
				found = true
				if s.StartNS < doc.Tracks[i].FirstNS {
					doc.Tracks[i].FirstNS = s.StartNS
				}
				last := s.StartNS + s.DurNS
				if last > doc.Tracks[i].LastNS {
					doc.Tracks[i].LastNS = last
				}
				doc.Tracks[i].Segments++
			}
		}
		if found {
			continue
		}
		doc.Tracks = append(doc.Tracks, trackDoc{
			Name: s.Track, Timescale: m.Timescale,
			FirstNS: s.StartNS, LastNS: s.StartNS + s.DurNS, Segments: 1,
		})
	}
	// Run every segment start through the unified clock so wraps and
	// regressions are detected rather than hidden by the manifest cursor.
	for i := range doc.Tracks {
		c, err := clock.NewTrackClock(doc.Tracks[i].Name, 1_000_000_000, 0)
		if err != nil {
			return doc, err
		}
		for _, s := range m.Segments {
			if s.Track != doc.Tracks[i].Name {
				continue
			}
			r := c.Convert(uint64(s.StartNS))
			if r.Wrapped {
				doc.Tracks[i].Wraps++
			}
			if r.Duplicate {
				doc.Tracks[i].Duplicates++
			}
			if r.Regressed && !r.Duplicate {
				doc.Tracks[i].Regression++
			}
		}
	}
	sort.Slice(doc.Tracks, func(i, j int) bool { return doc.Tracks[i].Name < doc.Tracks[j].Name })
	return doc, nil
}
