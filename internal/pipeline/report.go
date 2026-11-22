package pipeline

import (
	"fmt"
	"sort"

	"example.com/medialab/archive/internal/ladder"
	"example.com/medialab/archive/internal/model"
)

// Report is the machine-readable outcome of a run. Field names mirror the
// required demo output so the CLI can print them verbatim.
type Report struct {
	Reps               int
	Segments           int
	ManifestEntries    int
	LedgerEntries      int
	Resumed            bool
	Interrupted        bool
	Quarantined        int
	Duplicates         int
	Packets            int64
	AcceptedPackets    int64
	AudioUS            int64
	EncodedBytes       int64
	RecoveryRate       float64
	LossHoles          int64
	RecoveredLoss      int64
	CorruptHoles       int64
	Unrecoverable      int64
	EstimatedUS        int64
	SyncP95            int64
	PipelineP95        int64
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
		Segments:           len(r.latest),
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
	rep.BoundaryMaxDelta = r.boundaryDelta()
	rep.BoundaryMaxSkew = r.maxSkew()
	return rep
}

// boundaryDelta returns the largest difference between the start instants of
// the representations sharing a segment index.
func (r *Runner) boundaryDelta() int64 {
	byIndex := map[int]map[model.RepresentationID]int64{}
	for _, s := range r.latest {
		m := byIndex[s.Index]
		if m == nil {
			m = map[model.RepresentationID]int64{}
			byIndex[s.Index] = m
		}
		m[s.Rep] = s.StartUS
	}
	var max int64
	for _, m := range byIndex {
		var vals []int64
		for _, v := range m {
			vals = append(vals, v)
		}
		if len(vals) < 2 {
			continue
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		if d := vals[len(vals)-1] - vals[0]; d > max {
			max = d
		}
	}
	return max
}

func (r *Runner) maxSkew() int64 {
	var max int64
	for _, s := range r.latest {
		if s.BoundarySkew > max {
			max = s.BoundarySkew
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
		BoundaryOK:   rep.BoundaryMaxSkew <= TargetBoundaryDeltaMS*1000,
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
