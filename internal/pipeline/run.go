// Package pipeline wires the stream, normalize and txn subsystems into one
// crash-safe, deterministic render pipeline: fixture -> stream -> normalize
// -> txn. Every stage writes a WAL-protected artifact and the final timeline
// hashes to a stable SHA-256, so repeated runs are byte-identical.
package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"avflow/internal/model"
	"avflow/internal/normalize"
	"avflow/internal/stream"
	"avflow/internal/txn"
)

// Stage names used in the WAL and manifest.
const (
	StageStream    = "stream"
	StageNormalize = "normalize"
	StageTimeline  = "timeline"
)

// TimelinePoint is one clip boundary merged across every track.
type TimelinePoint struct {
	PTSNS       int64             `json:"pts_ns"`
	Track       uint32            `json:"track"`
	Seq         uint64            `json:"seq"`
	Kind        stream.TrackKind  `json:"kind"`
	SampleOK    bool              `json:"sample_aligned"`
	FrameOffset int64             `json:"frame_offset_ns"`
}

// TransitionWindow is one merged story-transition interval.
type TransitionWindow struct {
	Track    uint32 `json:"track"`
	StartNS  int64  `json:"start_ns"`
	EndNS    int64  `json:"end_ns"`
}

// Timeline is the merged, boundary-checked multi-track result.
type Timeline struct {
	Points      []TimelinePoint   `json:"points"`
	Transitions []TransitionWindow `json:"transitions"`
	Faults      []*model.Fault    `json:"faults"`
}

// Metrics captures the acceptance measurements for one run.
type Metrics struct {
	PacketCount          int     `json:"packet_count"`
	FaultPrecisionPkts   int     `json:"fault_precision_packets"`
	FalsePositives       int     `json:"false_positives"`
	P95WindowMS          float64 `json:"p95_window_ms"`
	ResumeMS             float64 `json:"resume_ms"`
	SHA256               string  `json:"sha256"`
	RepeatSHA256Match    bool    `json:"repeat_sha256_match"`
	RollbackOK           bool    `json:"rollback_ok"`
	FirstFault           *model.Fault `json:"first_fault,omitempty"`
}

// Config controls one pipeline execution.
type Config struct {
	WorkDir    string
	CrashAfter string // "", StageStream, StageNormalize or StageTimeline
}

// Run executes the pipeline against an encoded AVPK stream. The returned
// timeline and metrics reflect only the clean, committed prefix; non-
// recoverable faults (PTS regression) cause an explicit rollback.
func Run(data []byte, cfg Config) (*Timeline, *Metrics, error) {
	mgr, rep, err := openWorkspace(cfg.WorkDir)
	if err != nil {
		return nil, nil, err
	}
	defer mgr.Close()
	if rep != nil && len(rep.ReplayedStages) > 0 {
		// Recovery path: the caller measures resume time around Recover.
	}
	metrics := &Metrics{PacketCount: 0}
	// Stage 1: parse + index.
	idx, err := indexStage(mgr, data)
	if err != nil {
		return nil, metrics, handleUnrecoverable(mgr, err, metrics)
	}
	metrics.PacketCount = len(idx.Packets)
	metrics.FirstFault = idx.FirstFault
	if cfg.CrashAfter == StageStream {
		return nil, metrics, model.NewFault(model.FaultTruncated, 0, 0, 0, 0, false,
			"simulated crash after stream stage")
	}
	// Stage 2: normalize every clean track.
	norm, err := normalizeStage(mgr, idx)
	if err != nil {
		return nil, metrics, handleUnrecoverable(mgr, err, metrics)
	}
	if cfg.CrashAfter == StageNormalize {
		return nil, metrics, model.NewFault(model.FaultTruncated, 0, 0, 0, 0, false,
			"simulated crash after normalize stage")
	}
	// Stage 3: merge timeline and check all boundaries.
	tl := buildTimeline(idx, norm)
	tl.Faults = append(tl.Faults, idx.Faults...)
	tl.Faults = append(tl.Faults, norm.faults()...)
	if err := commitTimeline(mgr, tl); err != nil {
		return nil, metrics, err
	}
	metrics.SHA256 = hashTimeline(tl)
	return tl, metrics, nil
}

func openWorkspace(dir string) (*txn.Manager, *txn.RecoverReport, error) {
	// An existing manifest means this workspace is being resumed after a
	// crash: reconcile the WAL before doing anything else.
	if _, err := os.Stat(filepath.Join(dir, txn.ManifestFileName)); err == nil {
		mgr, rep, err := txn.Recover(dir)
		return mgr, rep, err
	}
	mgr, err := txn.NewManager(dir)
	return mgr, nil, err
}

func indexStage(mgr *txn.Manager, data []byte) (*stream.Index, error) {
	r, err := stream.NewReader(bytes.NewReader(data), stream.MaxPayloadDefault)
	if err != nil {
		return nil, err
	}
	idx, err := stream.BuildIndex(r)
	// Commit the index snapshot regardless, so the detected fault itself is
	// durable before the rollback decision.
	artifact, _ := json.Marshal(indexSnapshot(idx))
	if _, cerr := mgr.Stage(StageStream, "index.json", artifact); cerr != nil {
		return idx, cerr
	}
	if err != nil {
		f, ok := model.AsFault(err)
		if !ok || !f.Recoverable {
			return idx, err // PTS regression etc.: trigger rollback upstream
		}
	}
	return idx, nil
}

type normBundle struct {
	audio map[uint32]*normalize.AudioResult
	video map[uint32]*normalize.VideoResult
}

func (n *normBundle) faults() []*model.Fault {
	var out []*model.Fault
	for _, a := range n.audio {
		out = append(out, a.Faults...)
	}
	for _, v := range n.video {
		out = append(out, v.Faults...)
	}
	return out
}

func normalizeStage(mgr *txn.Manager, idx *stream.Index) (*normBundle, error) {
	bundle := &normBundle{audio: map[uint32]*normalize.AudioResult{}, video: map[uint32]*normalize.VideoResult{}}
	for _, desc := range idx.Tracks {
		packets := idx.TrackValid(desc.ID)
		switch desc.Kind {
		case stream.KindAudio:
			res, err := normalize.NormalizeAudio(desc, packets)
			if err != nil {
				return bundle, err
			}
			bundle.audio[desc.ID] = res
		case stream.KindVideo:
			res, err := normalize.NormalizeVideo(desc, packets)
			if err != nil {
				return bundle, err
			}
			bundle.video[desc.ID] = res
		}
	}
	artifact, _ := json.Marshal(bundle.faults())
	if _, err := mgr.Stage(StageNormalize, "normalize.json", artifact); err != nil {
		return bundle, err
	}
	return bundle, nil
}

func buildTimeline(idx *stream.Index, n *normBundle) *Timeline {
	tl := &Timeline{}
	for _, p := range idx.ValidPackets() {
		desc, ok := findDesc(idx.Tracks, p.Track)
		if !ok {
			continue
		}
		point := TimelinePoint{
			PTSNS: p.PTSNS, Track: p.Track, Seq: p.Seq, Kind: desc.Kind,
			SampleOK: true,
		}
		if desc.Kind == stream.KindAudio && desc.SampleRate > 0 {
			// Alignment is judged against the track's own sample grid; the
			// recorded offset is measured on that same grid, so a consistent
			// 44.1k track never produces spurious drift faults.
			point.SampleOK = model.SampleAligned(p.PTSNS, desc.SampleRate)
			point.FrameOffset = model.SampleOffsetNS(p.PTSNS, desc.SampleRate)
			if !point.SampleOK {
				tl.Faults = append(tl.Faults, model.NewFault(model.FaultBoundaryDrift,
					p.Track, p.Seq, p.PTSNS, p.EndPTSNS, true, "audio clip point off sample grid"))
			}
		}
		if desc.Kind == stream.KindVideo {
			point.FrameOffset = model.FrameGridOffsetNS(p.PTSNS, model.Rational{Num: 30, Den: 1})
		}
		if desc.Kind == stream.KindTransition {
			tl.Transitions = append(tl.Transitions, TransitionWindow{
				Track: p.Track, StartNS: p.PTSNS, EndNS: p.EndPTSNS})
		}
		tl.Points = append(tl.Points, point)
	}
	sort.Slice(tl.Points, func(i, j int) bool {
		if tl.Points[i].PTSNS != tl.Points[j].PTSNS {
			return tl.Points[i].PTSNS < tl.Points[j].PTSNS
		}
		return tl.Points[i].Track < tl.Points[j].Track
	})
	sort.Slice(tl.Transitions, func(i, j int) bool { return tl.Transitions[i].StartNS < tl.Transitions[j].StartNS })
	return tl
}

func commitTimeline(mgr *txn.Manager, tl *Timeline) error {
	artifact, err := json.Marshal(tl)
	if err != nil {
		return err
	}
	_, err = mgr.Stage(StageTimeline, "timeline.json", artifact)
	return err
}

// HashTimeline folds the deterministic timeline JSON into SHA-256. It is
// exported so the CLI adapter can compare repeated runs.
func HashTimeline(tl *Timeline) string {
	return hashTimeline(tl)
}

// hashTimeline folds the deterministic timeline JSON into SHA-256.
func hashTimeline(tl *Timeline) string {
	data, _ := json.Marshal(tl)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func handleUnrecoverable(mgr *txn.Manager, err error, m *Metrics) error {
	if f, ok := model.AsFault(err); ok && !f.Recoverable {
		// PTS regression and similar fatal defects trigger a rollback that
		// preserves the last committed checksum.
		kept, _ := mgr.Rollback(StageStream)
		m.RollbackOK = kept == mgr.Manifest().RollbackChecksum || mgr.Manifest().LastChecksum == kept
	}
	return err
}

func findDesc(tracks []stream.TrackDesc, id uint32) (stream.TrackDesc, bool) {
	for _, t := range tracks {
		if t.ID == id {
			return t, true
		}
	}
	return stream.TrackDesc{}, false
}

func indexSnapshot(idx *stream.Index) map[string]interface{} {
	return map[string]interface{}{
		"packets":      len(idx.Packets),
		"valid_prefix": len(idx.ValidPackets()),
		"faults":       idx.Faults,
		"first_fault":  idx.FirstFault,
	}
}
