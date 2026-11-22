package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"example.com/medialab/archive/internal/checkpoint"
	"example.com/medialab/archive/internal/ingest"
	"example.com/medialab/archive/internal/model"
	"example.com/medialab/archive/internal/recovery"
	"example.com/medialab/archive/internal/segment"
)

// pipelineState is the opaque checkpoint payload. It contains everything
// needed to continue without re-reading records already folded in.
type pipelineState struct {
	Cursor      int                     `json:"cursor"`
	Watermark   watermarkState          `json:"watermark"`
	Streams     []streamStateCheckpoint `json:"streams"`
	Quarantine  []model.Quarantine      `json:"quarantine,omitempty"`
	Dupes       int64                   `json:"dupes"`
	Conflicts   int64                   `json:"conflicts"`
	Sync        []int64                 `json:"sync_buckets"`
	P95         []int64                 `json:"p95_buckets"`
	Recovery    recoveryCheckpoint      `json:"recovery"`
	Segments    []segmentState          `json:"segments"`
	FinalSHA    map[string]string       `json:"final_sha"`
	OutputChain string                  `json:"output_chain"`
	OutputCount int                     `json:"output_count"`
	SegCount    int                     `json:"seg_count"`
	Counted     []string                `json:"counted"`
	BoundaryMax int64                   `json:"boundary_max"`
}

type watermarkState struct {
	MaxEndUS int64 `json:"max_end_us"`
	Set      bool  `json:"set"`
	WindowUS int64 `json:"window_us"`
}

type streamStateCheckpoint struct {
	Parser    ingest.ParserState         `json:"parser"`
	Frontier  int                        `json:"frontier"`
	Accepted  int64                      `json:"accepted"`
	AudioUS   int64                      `json:"audio_us"`
	CoverEnd  int64                      `json:"cover_end_us"`
	GapCursor int64                      `json:"gap_cursor"`
	GapEndUS  int64                      `json:"gap_end_us"`
	HasGap    bool                       `json:"has_gap_cursor"`
	HasEvent  bool                       `json:"has_event"`
	MaxSeq    int64                      `json:"max_seq"`
	Dedupe    map[uint32]string          `json:"dedupe"`
	Quarant   map[uint32]model.ErrorKind `json:"quarantined"`
	Events    []*model.Event             `json:"events"`
}

type recoveryCheckpoint struct {
	Loss             int64 `json:"loss"`
	Recovered        int64 `json:"recovered"`
	Corrupt          int64 `json:"corrupt"`
	RecoveredCorrupt int64 `json:"recovered_corrupt"`
	Unrecoverable    int64 `json:"unrecoverable"`
	EstimatedUS      int64 `json:"estimated_us"`
}

type segmentState struct {
	Rep       model.RepresentationID `json:"rep"`
	Index     int                    `json:"index"`
	Version   int                    `json:"version"`
	StartUS   int64                  `json:"start_us"`
	EndUS     int64                  `json:"end_us"`
	Frames    int                    `json:"frames"`
	SrcStart  int64                  `json:"src_start_us"`
	SrcEnd    int64                  `json:"src_end_us"`
	Skew      int64                  `json:"skew_us"`
	SHA       string                 `json:"sha"`
	Loss      int                    `json:"loss_fills"`
	Corrupt   int                    `json:"corrupt_fills"`
	Unrec     int                    `json:"unrecoverable_gaps"`
	SilenceUS int64                  `json:"silence_us"`
	MediaSHA  string                 `json:"media_sha,omitempty"`
}

// marshalPipelineState serialises the continuation state. Payload bytes are not
// carried: the events' durations and hashes are enough to rebuild outputs, and
// keeping bytes out bounds the checkpoint by the window, not the archive.
func (r *Runner) marshalPipelineState() (json.RawMessage, error) {
	maxEnd, set, window := r.wm.State()
	ps := pipelineState{
		Cursor:      r.cursor,
		Watermark:   watermarkState{MaxEndUS: maxEnd, Set: set, WindowUS: window},
		Quarantine:  r.quarantine,
		Dupes:       r.dupes,
		Conflicts:   r.conflicts,
		Sync:        r.syncHist.Buckets(),
		P95:         r.p95Hist.Buckets(),
		FinalSHA:    r.latestSHA,
		OutputChain: hex.EncodeToString(r.outputChain[:]),
		OutputCount: r.outputCount,
		SegCount:    r.segCount,
		Counted:     r.countedSlots(),
		BoundaryMax: r.boundaryMax,
		Recovery: recoveryCheckpoint{
			Loss: r.recoveryStats.LossHoles, Recovered: r.recoveryStats.RecoveredLoss,
			Corrupt: r.recoveryStats.CorruptHoles, RecoveredCorrupt: r.recoveryStats.RecoveredCorrupt,
			Unrecoverable: r.recoveryStats.Unrecoverable, EstimatedUS: r.recoveryStats.EstimatedUS,
		},
	}
	for _, st := range r.streams {
		cp := streamStateCheckpoint{
			Parser: st.parser.State(), Frontier: st.frontier,
			Accepted: st.accepted, AudioUS: st.audioUS,
			CoverEnd: st.coverEnd, HasEvent: st.hasEvent, MaxSeq: st.maxSeq,
			GapCursor: st.gapCursor, GapEndUS: st.gapEndUS, HasGap: st.hasGapCursor,
			Dedupe:  st.dedupe,
			Quarant: st.quarant,
			Events:  st.events,
		}
		ps.Streams = append(ps.Streams, cp)
	}
	for _, s := range r.latest {
		ps.Segments = append(ps.Segments, segmentState{
			Rep: s.Rep, Index: s.Index, Version: s.Version, StartUS: s.StartUS, EndUS: s.EndUS,
			Frames: s.FrameCount, SrcStart: s.SourceStart, SrcEnd: s.SourceEnd, Skew: s.BoundarySkew, SHA: s.OutputSHA256,
			Loss: s.LossFills, Corrupt: s.CorruptFills, Unrec: s.UnrecoverableGaps, SilenceUS: s.SilenceUS,
			MediaSHA: s.MediaSHA256,
		})
	}
	sort.Slice(ps.Segments, func(i, j int) bool {
		if ps.Segments[i].Rep != ps.Segments[j].Rep {
			return ps.Segments[i].Rep < ps.Segments[j].Rep
		}
		return ps.Segments[i].Index < ps.Segments[j].Index
	})
	return json.Marshal(ps)
}

// Restore loads a checkpoint and prepares the runner to continue from it. It
// returns the state so the caller can verify the config digest first.
func (r *Runner) Restore(st checkpoint.State) error {
	if st.ConfigDigest != r.cfg.Digest(r.reps) {
		return fmt.Errorf("pipeline: checkpoint was written for a different configuration")
	}
	if len(st.Pipeline) == 0 {
		return fmt.Errorf("pipeline: checkpoint has no pipeline state")
	}
	var ps pipelineState
	if err := json.Unmarshal(st.Pipeline, &ps); err != nil {
		return fmt.Errorf("pipeline: corrupt pipeline state: %w", err)
	}
	if ps.Cursor < 0 || ps.Cursor > len(r.log) {
		return fmt.Errorf("pipeline: cursor %d outside log of %d", ps.Cursor, len(r.log))
	}
	if len(ps.Streams) != len(r.streams) {
		return fmt.Errorf("pipeline: checkpoint has %d streams, run has %d", len(ps.Streams), len(r.streams))
	}
	r.cursor = ps.Cursor
	r.wm.Restore(ps.Watermark.MaxEndUS, ps.Watermark.Set, ps.Watermark.WindowUS)
	r.quarantine = ps.Quarantine
	r.dupes = ps.Dupes
	r.conflicts = ps.Conflicts
	if err := r.syncHist.Restore(ps.Sync); err != nil {
		return err
	}
	if err := r.p95Hist.Restore(ps.P95); err != nil {
		return err
	}
	r.recoveryStats = recovery.Stats{
		LossHoles: ps.Recovery.Loss, RecoveredLoss: ps.Recovery.Recovered,
		CorruptHoles: ps.Recovery.Corrupt, RecoveredCorrupt: ps.Recovery.RecoveredCorrupt,
		Unrecoverable: ps.Recovery.Unrecoverable, EstimatedUS: ps.Recovery.EstimatedUS,
	}
	chain, err := hex.DecodeString(ps.OutputChain)
	if err != nil || (ps.OutputChain != "" && len(chain) != sha256.Size) {
		return fmt.Errorf("pipeline: corrupt output chain")
	}
	copy(r.outputChain[:], chain)
	r.outputCount = ps.OutputCount
	r.segCount = ps.SegCount
	r.counted = map[string]bool{}
	for _, k := range ps.Counted {
		r.counted[k] = true
	}
	r.boundaryMax = ps.BoundaryMax
	for i, cp := range ps.Streams {
		st := r.streams[i]
		st.parser.Restore(cp.Parser)
		st.frontier = cp.Frontier
		st.accepted = cp.Accepted
		st.audioUS = cp.AudioUS
		st.coverEnd = cp.CoverEnd
		st.gapCursor = cp.GapCursor
		st.gapEndUS = cp.GapEndUS
		st.hasGapCursor = cp.HasGap
		st.hasEvent = cp.HasEvent
		st.maxSeq = cp.MaxSeq
		st.dedupe = cp.Dedupe
		if st.dedupe == nil {
			st.dedupe = map[uint32]string{}
		}
		st.quarant = cp.Quarant
		if st.quarant == nil {
			st.quarant = map[uint32]model.ErrorKind{}
		}
		st.events = cp.Events
	}
	r.latest = map[string]model.Segment{}
	r.latestSHA = map[string]string{}
	for _, s := range ps.Segments {
		r.latest[key(s.Rep, s.Index)] = model.Segment{
			Rep: s.Rep, Index: s.Index, Version: s.Version, StartUS: s.StartUS, EndUS: s.EndUS,
			FrameCount: s.Frames, SourceStart: s.SrcStart, SourceEnd: s.SrcEnd, BoundarySkew: s.Skew, OutputSHA256: s.SHA,
			LossFills: s.Loss, CorruptFills: s.Corrupt, UnrecoverableGaps: s.Unrec, SilenceUS: s.SilenceUS,
			MediaSHA256: s.MediaSHA,
		}
		r.latestSHA[key(s.Rep, s.Index)] = s.SHA
	}
	// Rebuild the chained digests and per-slot cursors so duplicate commits
	// after the restart are idempotent.
	if r.manifest != nil {
		if err := r.manifest.Restore(st.ManifestChain, st.ManifestCount); err != nil {
			return err
		}
		for _, s := range ps.Segments {
			r.manifest.SetCursor(s.Rep, s.Index, s.Version, s.SHA)
		}
	}
	if r.ledger != nil {
		if err := r.ledger.Restore(st.LedgerDigest, r.ledgerCount(st)); err != nil {
			return err
		}
		for _, s := range ps.Segments {
			r.ledger.Remember(s.Rep, s.Index, s.Version, s.SHA)
		}
	}
	if err := r.versions.Import(st.TimelineVersions, st.MaxVersion, st.ReplayedSegments); err != nil {
		return err
	}
	// The output chain resumes from the digest the previous process ended with.
	// Committing the same segments again must therefore be idempotent, which the
	// manifest and ledger writers guarantee by their cursor state.
	return nil
}

// ledgerCount returns how many ledger lines have been written so far. Every
// committed segment version produces exactly one line, so the manifest count
// is the ledger count as well.
func (r *Runner) ledgerCount(st checkpoint.State) int { return st.ManifestCount }

// segmentOutputDigest hashes the canonical description of a segment. It does
// not include wall-clock time or map order, so the same segment always yields
// the same digest in any process.
func segmentOutputDigest(e *segment.Entry) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1|rep=%s|index=%d|ver=%d|%d-%d|frames=%d|src=%s:%d-%d|%d-%d|skew=%d|silence=%d|ops=%v|replaces=%d",
		e.Rep, e.Index, e.Version, e.StartUS, e.EndUS, e.FrameCount,
		e.SourceStream, e.SourceSeqMin, e.SourceSeqMax, e.SourceStartUS, e.SourceEndUS,
		e.BoundarySkewUS, e.SilenceUS, e.RecoveryOps, e.ReplacesVersion)
	return hex.EncodeToString(h.Sum(nil))
}
