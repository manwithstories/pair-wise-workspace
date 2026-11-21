package pipeline

import (
	"encoding/json"
	"fmt"
	"os"

	"mediad/internal/fixture"
	"mediad/internal/manifest"
	"mediad/internal/model"
	"mediad/internal/repair"
	"mediad/internal/report"
	"mediad/internal/rtp"
	"mediad/internal/txn"
)

type runner struct {
	opt   DemoOptions
	paths map[string]string
	txm   *txn.Manager
	res   *Result

	container  model.Container
	lossReport rtp.LossReport
	rtpStats   rtp.Stats
	rtpTrack   *model.Track
	repairs    []repair.Report
	mismatches int
	done       map[txn.Stage]bool
}

// execute runs one stage, reusing a verified checkpoint when resuming.
func (r *runner) execute(s txn.Stage) error {
	if r.done == nil {
		r.done = map[txn.Stage]bool{}
	}
	if r.done[s] {
		return nil
	}
	committed := r.txm.Committed()
	reuse := r.opt.Resume && has(committed, s)
	switch s {
	case txn.StageProbe:
		if reuse {
			data, err := os.ReadFile(r.txm.ArtifactPath(s))
			if err != nil {
				return model.Wrap(model.KindRecovery, "pipeline", "read probe checkpoint", err)
			}
			if err := json.Unmarshal(data, &r.container); err != nil {
				return model.Wrap(model.KindRecovery, "pipeline", "decode probe checkpoint", err)
			}
			r.res.Notes = append(r.res.Notes, "probe stage reused from a checksum-verified checkpoint")
		} else {
			if err := probeAll(r.paths["mp4"], r.paths["mxf"], &r.container, r.res); err != nil {
				return err
			}
			if _, err := r.txm.Commit(s, mustJSON(r.container)); err != nil {
				return err
			}
		}
	case txn.StageRTP:
		if reuse {
			data, err := os.ReadFile(r.txm.ArtifactPath(s))
			if err == nil {
				var payload struct {
					Stats rtp.Stats      `json:"stats"`
					Loss  rtp.LossReport `json:"loss"`
					Track model.Track    `json:"track"`
				}
				if json.Unmarshal(data, &payload) == nil {
					r.rtpStats, r.lossReport = payload.Stats, payload.Loss
					track := payload.Track
					r.rtpTrack = &track
				}
			}
			break
		}
		rep, stats, faults, track, err := runRTP(r.opt, r.res)
		if err != nil {
			return err
		}
		r.lossReport, r.rtpStats = rep, stats
		r.rtpTrack = track
		r.res.InjectedFaults += faults
		if _, err := r.txm.Commit(s, mustJSON(map[string]any{
			"stats": stats, "loss": rep, "track": track,
		})); err != nil {
			return err
		}
	case txn.StageRepair:
		if reuse {
			data, err := os.ReadFile(r.txm.ArtifactPath(s))
			if err == nil {
				json.Unmarshal(data, &r.repairs)
			}
			// The container must carry repaired timestamps for indexing.
			for i := range r.container.Tracks {
				if _, err := repair.Track(&r.container.Tracks[i], repair.Options{NominalFrameRate: r.container.Tracks[i].FrameRate}); err != nil {
					return err
				}
			}
			break
		}
		r.repairs = r.repairs[:0]
		for i := range r.container.Tracks {
			t := &r.container.Tracks[i]
			rep, err := repair.Track(t, repair.Options{NominalFrameRate: t.FrameRate})
			if err != nil {
				return err
			}
			r.repairs = append(r.repairs, rep)
		}
		// The reconstructed RTP stream is handed to the same repair stage.
		// Its 32-bit media clock wraps, so wrap handling is enabled and the
		// wrap count becomes part of the acceptance report.
		if r.rtpTrack != nil {
			// Drift is not measurable on a lossy stream: the samples span
			// more wall time than their count implies, which would look like
			// a clock error. Wrap handling still applies, so the 32-bit
			// field is repaired and counted.
			rep, err := repair.Track(r.rtpTrack, repair.Options{
				WrapTicks: 1 << 32,
				ClockRate: r.rtpTrack.Timescale,
			})
			if err != nil {
				return err
			}
			r.repairs = append(r.repairs, rep)
		}
		if _, err := r.txm.Commit(s, mustJSON(r.repairs)); err != nil {
			return err
		}
	case txn.StageIndex:
		bad, err := buildIndexes(r.txm, r.container, r.opt, r.res)
		if err != nil {
			return err
		}
		r.mismatches = bad
		if hasKind(r.opt.Faults, fixture.FaultCorruptLog) && !reuse {
			if r.mismatches, err = recoverCorruptIndex(r.txm, r.container, r.opt, r.res); err != nil {
				return err
			}
		}
	case txn.StageManifest:
		return r.commitManifest()
	}
	r.done[s] = true
	return nil
}

// simulateCrash writes the begin/data records of the next stage and a torn
// frame, then leaves the process "dead" without committing that stage.
func (r *runner) simulateCrash() error {
	limit := stageIndex(r.opt.InterruptAfter)
	if limit+1 < len(txn.Order) {
		next := txn.Order[limit+1]
		if _, err := r.txm.WAL().Append(txn.KindBegin, string(next), nil); err != nil {
			return model.Wrap(model.KindRecovery, "pipeline", "simulate interruption", err)
		}
		if _, err := r.txm.WAL().Append(txn.KindData, string(next), []byte(`{"partial":true}`)); err != nil {
			return model.Wrap(model.KindRecovery, "pipeline", "simulate interruption", err)
		}
	}
	if err := r.txm.AppendTornTail(); err != nil {
		return model.Wrap(model.KindRecovery, "pipeline", "simulate interruption", err)
	}
	return nil
}

func (r *runner) finish() error {
	if r.res.Manifest.Version == 0 {
		return model.New(model.KindInternal, "pipeline", "run ended before the manifest stage")
	}
	return nil
}

// hydrate fills stage outputs that this process did not compute itself, so a
// run that resumes at the manifest stage still reports the counters recorded
// by the stages it reused.
func (r *runner) hydrate() {
	if len(r.repairs) == 0 {
		if data, err := os.ReadFile(r.txm.ArtifactPath(txn.StageRepair)); err == nil {
			json.Unmarshal(data, &r.repairs)
		}
	}
	if r.rtpStats.Received == 0 && r.lossReport.TotalLost == 0 && r.lossReport.RecoveredSamples == 0 {
		if data, err := os.ReadFile(r.txm.ArtifactPath(txn.StageRTP)); err == nil {
			var payload struct {
				Stats rtp.Stats      `json:"stats"`
				Loss  rtp.LossReport `json:"loss"`
			}
			if json.Unmarshal(data, &payload) == nil {
				r.rtpStats, r.lossReport = payload.Stats, payload.Loss
			}
		}
	}
}

func (r *runner) commitManifest() error {
	r.hydrate()
	targets := targetSpecs()
	man, err := manifest.Build(r.opt.Seed, r.container.Format, targets, r.container.Tracks)
	if err != nil {
		return err
	}
	quality, err := measureQuality(r.opt.Seed, r.container, r.paths)
	if err != nil {
		return err
	}
	targetKbps := 0.0
	for _, t := range targets {
		targetKbps += t.TargetBitrateKbps
	}
	repairCounts := report.RepairCounts{}
	for _, rep := range r.repairs {
		repairCounts.Wraps += rep.Wraps
		repairCounts.Duplicates += rep.Duplicates
		repairCounts.Adjusted += rep.Adjusted
		if d := rep.DriftPPM; d < 0 {
			if -d > repairCounts.WorstDriftPPM {
				repairCounts.WorstDriftPPM = -d
			}
		} else if d > repairCounts.WorstDriftPPM {
			repairCounts.WorstDriftPPM = d
		}
	}
	r.res.Manifest = man
	r.res.Summary = report.Builder{
		Seed:              fmt.Sprintf("%d", r.opt.Seed),
		TargetBitrateKbps: targetKbps,
		Manifest:          man,
		Quality:           quality,
		Loss: report.LossCounts{
			Recovered:     r.lossReport.RecoveredSamples,
			Unrecoverable: r.lossReport.UnrecoverableSamples,
			Duplicates:    r.rtpStats.Duplicates,
			Reordered:     r.rtpStats.Reordered,
		},
		IndexMismatches: r.mismatches,
		RollbackStage:   r.res.RollbackStage,
		ResumeStage:     r.res.ResumeStage,
		InjectedFaults:  r.res.InjectedFaults,
		Repair:          repairCounts,
	}.Build()
	_, err = r.txm.Commit(txn.StageManifest, mustJSON(r.res.Summary))
	return err
}

// buildIndexes rebuilds and verifies every track's index, then commits it.
