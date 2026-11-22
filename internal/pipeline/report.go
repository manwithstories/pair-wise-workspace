package pipeline

import (
	"fmt"

	"example.com/medialab/archive/internal/ladder"
	"example.com/medialab/archive/internal/model"
)

// Report is the machine-readable outcome of a run. Field names mirror the
// required demo output so the CLI can print them verbatim.
type Report struct {
	Reps            int
	Segments        int
	ManifestEntries int
	LedgerEntries   int
	Resumed         bool
	Interrupted     bool
	Quarantined     int
	Duplicates      int
	Packets         int64
	AcceptedPackets int64
	AudioUS         int64
	EncodedBytes    int64
	RecoveryRate    float64
	LossHoles       int64
	RecoveredLoss   int64
	CorruptHoles    int64
	Unrecoverable   int64
	EstimatedUS     int64
	SyncP95         int64
	PipelineP95     int64
	// BoundaryGridDelta is the difference between the cut instants of the
	// representations (delivery alignment; always 0 on a shared grid).
	BoundaryGridDelta int64
	// BoundaryMaxDelta is the worst residual offset between the underlying
	// packet edges of two renditions at a common boundary.
	BoundaryMaxDelta   int64
	BoundaryMaxSkew    int64
	MaxVersion         int
	ReplayedSegments   int
	OutputSHA256       string
	OutputSHA256Match  bool
	ReplayVersionCount int
	Elapsed            int64
	// Verify evaluates the acceptance targets.
	Verify VerifyResult
}

// VerifyResult holds the threshold checks.
type VerifyResult struct {
	SyncOK       bool
	RecoveryOK   bool
	PipelineOK   bool
	BoundaryOK   bool
	ResumeOK     bool
	ReplayOK     bool
	QuarantineOK bool
}

// Passed reports whether every checked target held.
func (v VerifyResult) Passed() bool {
	return v.SyncOK && v.RecoveryOK && v.PipelineOK && v.BoundaryOK && v.ResumeOK && v.ReplayOK && v.QuarantineOK
}

// Targets are the acceptance thresholds from the brief.
const (
	TargetSyncP95MS       = 40
	TargetRecoveryRate    = 0.85
	TargetPipelineP95MS   = 300
	TargetBoundaryDeltaMS = 20
	TargetReplayVersions  = 2
)

// BuildReport summarises the run.
func (r *Runner) BuildReport() Report {
	rep := Report{
		Reps:               len(r.streams),
		Segments:           r.segCount,
		Quarantined:        len(r.quarantine),
		Duplicates:         int(r.dupes),
		SyncP95:            r.syncHist.Percentile(95),
		PipelineP95:        r.p95Hist.Percentile(95),
		MaxVersion:         r.versions.MaxVersion(),
		ReplayedSegments:   r.versions.ReplayedSegments(),
		OutputSHA256:       r.OutputDigest(),
		RecoveryRate:       r.recoveryStats.Rate(),
		LossHoles:          r.recoveryStats.LossHoles,
		RecoveredLoss:      r.recoveryStats.RecoveredLoss,
		CorruptHoles:       r.recoveryStats.CorruptHoles,
		Unrecoverable:      r.recoveryStats.Unrecoverable,
		EstimatedUS:        r.recoveryStats.EstimatedUS,
		Interrupted:        r.interrupted,
		ReplayVersionCount: r.versions.MaxVersion(),
	}
	if r.manifest != nil {
		rep.ManifestEntries = r.manifest.Count()
	}
	if r.ledger != nil {
		rep.LedgerEntries = r.ledger.Count()
	}
	for _, st := range r.streams {
		rep.Packets += st.parser.Counters.Total
		rep.AcceptedPackets += st.accepted
		rep.AudioUS += st.audioUS
	}
	rep.EncodedBytes = r.encodedBytes
	// The delivery boundary delta is the cut alignment. The packet-edge skew is
	// a separate record: AAC frames of 21333us cannot land exactly on a 10s
	// grid, and the requirement is to record that deviation, not hide it.
	rep.BoundaryGridDelta = r.boundaryMax
	rep.BoundaryMaxDelta = r.boundaryMax
	rep.BoundaryMaxSkew = r.maxSkew()
	return rep
}

// countedSlots exports the slots counted so far. Only the newest slots per
// representation can still be replayed, so older entries are dropped.
func (r *Runner) countedSlots() []string {
	out := make([]string, 0, len(r.counted))
	var maxIdx = map[model.RepresentationID]int{}
	for k := range r.counted {
		out = append(out, k)
	}
	for _, st := range r.streams {
		maxIdx[st.spec.rep.ID] = st.frontier
	}
	kept := out[:0]
	for _, k := range out {
		var rep model.RepresentationID
		var idx int
		if _, err := fmt.Sscanf(k, "%s", &rep); err != nil {
			continue
		}
		slash := -1
		for i := len(k) - 1; i >= 0; i-- {
			if k[i] == '/' {
				slash = i
				break
			}
		}
		if slash < 0 {
			continue
		}
		rep = model.RepresentationID(k[:slash])
		if _, err := fmt.Sscanf(k[slash+1:], "%d", &idx); err != nil {
			continue
		}
		if idx >= maxIdx[rep]-2 {
			kept = append(kept, k)
		}
	}
	return kept
}

// maxSkew returns the worst recorded boundary skew among tracked slots.
func (r *Runner) maxSkew() int64 {
	var max int64
	for _, v := range r.skewBySlot {
		if v > max {
			max = v
		}
	}
	return max
}

// Verify applies the acceptance thresholds. resumeMatch is the hash comparison
// result supplied by the caller (a clean run versus the resumed run).
func (r *Runner) Verify(rep Report, resumeMatch bool) VerifyResult {
	return VerifyResult{
		SyncOK:       rep.SyncP95 <= TargetSyncP95MS,
		RecoveryOK:   rep.RecoveryRate >= TargetRecoveryRate,
		PipelineOK:   rep.PipelineP95 <= TargetPipelineP95MS,
		BoundaryOK:   rep.BoundaryGridDelta <= TargetBoundaryDeltaMS*1000,
		ResumeOK:     resumeMatch,
		ReplayOK:     rep.ReplayVersionCount >= TargetReplayVersions,
		QuarantineOK: rep.Quarantined > 0 || rep.CorruptHoles == 0,
	}
}

// ExpectedLadder returns the representations for reporting.
func (r *Runner) ExpectedLadder() []model.Representation {
	reps, err := ladder.ParseBitrates(r.cfg.Bitrates)
	if err != nil {
		return nil
	}
	for i := range reps {
		for _, st := range r.streams {
			if st.spec.rep.ID == reps[i].ID {
				reps[i].Codec = st.spec.rep.Codec
				reps[i].FrameDurationUS = st.spec.rep.FrameDurationUS
			}
		}
	}
	return reps
}

// String renders one report line for humans.
func (rep Report) String() string {
	return fmt.Sprintf("reps=%d segments=%d quarantined=%d dupes=%d rate=%.3f replay_v=%d sha=%s",
		rep.Reps, rep.Segments, rep.Quarantined, rep.Duplicates, rep.RecoveryRate, rep.MaxVersion, rep.OutputSHA256)
}
