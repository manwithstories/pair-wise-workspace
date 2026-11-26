// Package pipeline wires scan, repair, recovery, indexing, and verification.
package pipeline

import (
	"math/rand"
	"os"

	"mediad/internal/audio"
	clockpkg "mediad/internal/clock"
	"mediad/internal/index"
	"mediad/internal/media"
	"mediad/internal/metrics"
	"mediad/internal/mp4"
	"mediad/internal/recover"
	"mediad/internal/verify"
	"mediad/internal/video"
)

// Options controls one deterministic processing run.
type Options struct {
	Seed  int64
	Loss  float64
	Burst int
	// Salvage allows truncated mdat boundary clamping.
	Salvage bool
}

// SampleSummary is the category-level report for one sample.
type SampleSummary struct {
	Name             string   `json:"name"`
	Category         string   `json:"category"`
	Input            string   `json:"input"`
	Output           string   `json:"output,omitempty"`
	Recoverable      bool     `json:"recoverable"`
	Tracks           int      `json:"tracks"`
	FaultCount       int      `json:"fault_count"`
	TimestampFixes   int      `json:"timestamp_fixes"`
	OffsetFixes      int      `json:"offset_fixes"`
	EditFixes        int      `json:"edit_fixes"`
	LostPackets      int      `json:"lost_packets"`
	Unrecovered      int      `json:"unrecovered_packets"`
	Retransmissions  int      `json:"retransmissions"`
	RecoveryPct      float64  `json:"recovery_pct"`
	IndexSeekMiss    int      `json:"index_seek_miss"`
	BoundaryMismatch int      `json:"boundary_mismatch"`
	Notes            []string `json:"notes,omitempty"`
}

// Result contains a processed sample and its independent verification.
type Result struct {
	Summary SampleSummary   `json:"summary"`
	Verify  *verify.Report  `json:"verify,omitempty"`
	Metrics metrics.Metrics `json:"metrics,omitempty"`
	Index   *index.Index    `json:"-"`
}

// CLIError carries the required process exit code.
type CLIError struct {
	Code int
	Err  error
}

func (e *CLIError) Error() string { return e.Err.Error() }
func (e *CLIError) Unwrap() error { return e.Err }

func unrecoverable(err error) error {
	return &CLIError{Code: 2, Err: err}
}

func internal(err error) error {
	return &CLIError{Code: 1, Err: err}
}

// ProcessFile reads, repairs, recovers, indexes, writes, and independently
// verifies one sample.
func ProcessFile(name, category, input, output string, opts Options) (*Result, error) {
	p, _, parseFaults, err := mp4.ReadPath(input, opts.Salvage)
	if err != nil {
		f := media.AsFaultOrInternal(err)
		if f.Code == media.CodeInternal {
			return nil, internal(err)
		}
		return nil, unrecoverable(err)
	}

	sum := SampleSummary{
		Name: name, Category: category, Input: input, Output: output,
		Recoverable: true, FaultCount: len(parseFaults) + len(p.Faults),
	}
	for _, t := range p.Tracks {
		sum.Tracks++
		if t.Kind == media.KindAudio {
			ar := audio.Validate(t)
			sum.FaultCount += len(ar.Faults)
		}
		if t.Kind == media.KindVideo {
			vr := video.Validate(t)
			sum.FaultCount += len(vr.Faults)
		}
	}
	for _, t := range p.Tracks {
		if t.Failed {
			sum.Recoverable = false
			return &Result{Summary: sum}, unrecoverable(media.NewFault(media.CodeUnrecoverableTrack, "track %d failed: %s", t.ID, t.FailureReason).WithTrack(t.ID).Fatal())
		}
	}

	// Rebuild a missing stss from in-band sync markers before repair/write.
	// Existing stss flags are preserved and authoritative when present.
	if inputFile, err := os.Open(input); err == nil {
		if st, statErr := inputFile.Stat(); statErr == nil {
			_, syncFaults := index.RebuildVerified(p, inputFile, st.Size())
			for _, f := range syncFaults {
				if f != nil && f.Code == media.CodeMissingIndex {
					p.Faults = append(p.Faults, f)
					sum.FaultCount++
				}
			}
		}
		_ = inputFile.Close()
	}

	rep := mp4.RepairTimetables(p)
	sum.TimestampFixes = rep.TimestampFixes
	sum.OffsetFixes = rep.OffsetFixes
	sum.EditFixes = rep.EditFixes
	sum.FaultCount += len(rep.Faults)

	if output != "" {
		if err := mp4.WriteFile(output, p); err != nil {
			return nil, internal(err)
		}
	}

	var vrep *verify.Report
	sha := ""
	seekMiss, mismatch := 0, 0
	if output != "" {
		vrep, err = verify.VerifyFile(output)
		if err != nil {
			return nil, unrecoverable(err)
		}
		sha = vrep.OutputSHA256
		seekMiss = vrep.IndexSeekMiss
		mismatch = vrep.BoundaryMismatch
		sum.IndexSeekMiss = seekMiss
		sum.BoundaryMismatch = mismatch
	}

	recoveryReports, idx, gapNS, err := simulateOutput(output, opts)
	if err != nil {
		return nil, internal(err)
	}
	for _, r := range recoveryReports {
		sum.LostPackets += r.LostPackets
		sum.Unrecovered += r.UnrecoveredPackets
		sum.Retransmissions += r.Retransmissions
	}
	if sum.LostPackets > 0 {
		sum.RecoveryPct = 100 * float64(sum.LostPackets-sum.Unrecovered) / float64(sum.LostPackets)
	} else {
		sum.RecoveryPct = 100
	}

	m := metrics.Compute(gapNS, recoveryReports, seekMiss, mismatch, sha)
	sum.Notes = append(sum.Notes, "shared-clock fanout and independent per-track checkpoints applied")
	return &Result{Summary: sum, Verify: vrep, Metrics: m, Index: idx}, nil
}

func simulateOutput(output string, opts Options) ([]*recover.Report, *index.Index, int64, error) {
	if output == "" {
		return nil, nil, 0, nil
	}
	st, err := os.Stat(output)
	if err != nil {
		return nil, nil, 0, err
	}
	f, err := os.Open(output)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()
	p, _, _, err := mp4.ReadProject(f, st.Size(), false)
	if err != nil {
		return nil, nil, 0, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, nil, 0, err
	}
	idx, _ := index.RebuildVerified(p, f, st.Size())
	if err := idx.WriteJSON(output + ".index.json"); err != nil {
		// An incomplete index must never be published; this is a processing failure.
		return nil, idx, 0, err
	}

	rng := rand.New(rand.NewSource(opts.Seed))
	cfg := recover.DefaultConfig(opts.Loss, opts.Burst)
	var reports []*recover.Report
	for _, t := range p.Tracks {
		reports = append(reports, recover.RunTrack(t, idx, rng, cfg))
	}
	fanout, _ := clockpkg.New(p, clockpkg.DefaultTimescale)
	var gap int64
	var video, audio uint32
	for _, t := range p.Tracks {
		if t.Kind == media.KindVideo && video == 0 {
			video = t.ID
		}
		if t.Kind == media.KindAudio && audio == 0 {
			audio = t.ID
		}
	}
	if video != 0 && audio != 0 {
		gap = fanout.BoundaryGapNS(video, audio)
	}
	return reports, idx, gap, nil
}
