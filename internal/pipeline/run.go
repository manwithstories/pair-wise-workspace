// Package pipeline wires the probe, RTP, repair, index, manifest and report
// subsystems together and records every stage through the WAL.
package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/clock"
	"mediad/internal/fixture"
	"mediad/internal/index"
	"mediad/internal/manifest"
	"mediad/internal/model"
	"mediad/internal/normalize"
	"mediad/internal/probe"
	"mediad/internal/repair"
	"mediad/internal/report"
	"mediad/internal/rtp"
	"mediad/internal/txn"
)

// FixtureNames are the files the demo writes into its working directory.
const (
	MP4Name = "sample.mp4"
	MXFName = "sample.mxf"
)

// The demo's media is fixed and deliberately small: this is a demonstration
// of the library, not a transcoder. Sizes are chosen so a healthy delivery
// lands 2% under its target, matching the delivery spec's safety margin.
const (
	videoFrames      = 120
	videoDelta       = 1001
	videoTimescale   = 30000
	videoBytes       = 100000
	audioFrames      = 192
	audioDelta       = 1000
	audioTimescale   = 48000
	audioBytes       = 667
	mxfEssenceBytes  = 100000
	headroomFraction = 0.02
)

// DemoOptions configures the demo run.
type DemoOptions struct {
	Seed           int64
	Faults         []fixture.FaultKind
	BandwidthMbps  []float64
	InterruptAfter txn.Stage
	Resume         bool
	Dir            string
	SeeksPerTrack  int
	MaxBurst       int
}

// Result is everything the CLI prints and gates on.
type Result struct {
	Summary        report.Summary
	Manifest       manifest.Manifest
	RollbackStage  string
	ResumeStage    string
	InjectedFaults int
	Notes          []string
	ReusedStages   []string
}

// Run executes the complete demo pipeline. For a given seed and fault list it
// is deterministic: repeated runs produce the same manifest digest.
func Run(opt DemoOptions) (Result, error) {
	var res Result
	if err := opt.validate(); err != nil {
		return res, err
	}
	mp4Path := filepath.Join(opt.Dir, MP4Name)
	mxfPath := filepath.Join(opt.Dir, MXFName)
	if err := writeFixtures(opt.Seed, mp4Path, mxfPath); err != nil {
		return res, err
	}
	txnDir := filepath.Join(opt.Dir, "txn")
	if !opt.Resume {
		if err := os.RemoveAll(txnDir); err != nil {
			return res, model.Wrap(model.KindInternal, "pipeline", "reset transaction directory", err)
		}
	}

	txm, err := txn.Open(txnDir, 0)
	if err != nil {
		return res, model.Wrap(model.KindRecovery, "pipeline", "open transaction log", err)
	}
	defer txm.Close()
	if dirty, reason := txm.CorruptTail(); dirty && !opt.Resume {
		res.Notes = append(res.Notes, "discarded a corrupt WAL tail: "+reason)
	}

	r := &runner{opt: opt, paths: map[string]string{"mp4": mp4Path, "mxf": mxfPath}, txm: txm, res: &res}
	limit := -1
	if opt.InterruptAfter != "" {
		limit = stageIndex(opt.InterruptAfter)
	}
	if opt.Resume && limit < 0 {
		// A plain resume over an existing log: recover first (in case the
		// previous process died) and record which stages can be reused.
		if _, err := txm.Recover(); err != nil {
			return res, err
		}
		res.RollbackStage = txm.RollbackStage()
		committed := txm.Committed()
		for _, s := range txn.Order {
			if _, ok := committed[s]; ok {
				res.ReusedStages = append(res.ReusedStages, string(s))
			}
		}
	}

	// Phase 1: run through the stage after which the process "dies".
	for i, s := range txn.Order {
		if limit >= 0 && i > limit {
			break
		}
		if err := r.execute(s); err != nil {
			return res, err
		}
	}
	if limit < 0 {
		return res, r.finish()
	}

	// Phase 2: die mid-commit. The WAL keeps a partial record and a torn
	// tail, and the next stage's artifact was never written.
	if err := r.simulateCrash(); err != nil {
		return res, err
	}
	if !opt.Resume {
		return res, model.New(model.KindRecovery, "pipeline",
			fmt.Sprintf("interrupted after %s: artifacts for the next stage are incomplete; rerun with --resume to continue", opt.InterruptAfter))
	}

	// Phase 3: reopen and recover, reusing only checksum-verified stages.
	if err := txm.Close(); err != nil {
		return res, model.Wrap(model.KindRecovery, "pipeline", "close transaction log", err)
	}
	txm2, err := txn.Open(txnDir, 0)
	if err != nil {
		return res, model.Wrap(model.KindRecovery, "pipeline", "reopen transaction log", err)
	}
	defer txm2.Close()
	if dirty, reason := txm2.CorruptTail(); dirty {
		res.Notes = append(res.Notes, "recovery discarded a torn WAL tail: "+reason)
	}
	resumeFrom, err := txm2.Recover()
	if err != nil {
		return res, err
	}
	res.RollbackStage = txm2.RollbackStage()
	if res.RollbackStage == "" {
		res.RollbackStage = string(opt.InterruptAfter)
	}
	if resumeFrom == "" {
		// Every committed stage verified, but the crashed stage never
		// committed, so its successor is the one to resume at.
		resumeFrom = txn.Order[limit+1]
	}
	res.ResumeStage = string(resumeFrom)
	committed := txm2.Committed()
	for _, s := range txn.Order {
		if _, ok := committed[s]; ok {
			res.ReusedStages = append(res.ReusedStages, string(s))
		}
	}
	r.txm = txm2

	// Phase 4: continue from the first uncommitted stage.
	for i, s := range txn.Order {
		if i < stageIndex(resumeFrom) {
			continue
		}
		if err := r.execute(s); err != nil {
			return res, err
		}
	}
	return res, r.finish()
}

func (o *DemoOptions) validate() error {
	if o.Dir == "" {
		return model.New(model.KindInvalidInput, "pipeline", "working directory is required")
	}
	if o.SeeksPerTrack <= 0 {
		o.SeeksPerTrack = 11
	}
	if o.MaxBurst <= 0 {
		o.MaxBurst = 8
	}
	if o.InterruptAfter != "" && !validStage(o.InterruptAfter) {
		return model.New(model.KindInvalidInput, "pipeline",
			fmt.Sprintf("unknown interrupt stage %q", o.InterruptAfter))
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return model.Wrap(model.KindInternal, "pipeline", "create working directory", err)
	}
	return nil
}

// runner carries state across stages of one process run.
type runner struct {
	opt   DemoOptions
	paths map[string]string
	txm   *txn.Manager
	res   *Result

	container  model.Container
	lossReport rtp.LossReport
	rtpStats   rtp.Stats
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
		if len(r.opt.Faults) > 0 && !reuse {
			rep, stats, faults, err := runRTP(r.opt, r.res)
			if err != nil {
				return err
			}
			r.lossReport, r.rtpStats = rep, stats
			r.res.InjectedFaults += faults
			if _, err := r.txm.Commit(s, mustJSON(map[string]any{"stats": stats, "loss": rep})); err != nil {
				return err
			}
		} else if reuse {
			data, err := os.ReadFile(r.txm.ArtifactPath(s))
			if err == nil {
				var payload struct {
					Stats rtp.Stats      `json:"stats"`
					Loss  rtp.LossReport `json:"loss"`
				}
				if json.Unmarshal(data, &payload) == nil {
					r.rtpStats, r.lossReport = payload.Stats, payload.Loss
				}
			}
		} else {
			// No faults: the stage still commits so the WAL records that it
			// ran and produced no loss.
			if _, err := r.txm.Commit(s, mustJSON(map[string]any{"stats": r.rtpStats, "loss": r.lossReport})); err != nil {
				return err
			}
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

func (r *runner) commitManifest() error {
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
func buildIndexes(txm *txn.Manager, container model.Container, opt DemoOptions, res *Result) (int, error) {
	indexes := make([]index.TrackIndex, 0, len(container.Tracks))
	mismatches := 0
	for _, t := range container.Tracks {
		idx, err := index.Rebuild(t)
		if err != nil {
			return 0, err
		}
		bad, msgs, err := index.Verify(t, idx, opt.SeeksPerTrack)
		if err != nil {
			return 0, err
		}
		mismatches += bad
		for _, m := range msgs {
			res.Notes = append(res.Notes, fmt.Sprintf("track %d %s", t.ID, m))
		}
		indexes = append(indexes, idx)
	}
	if _, err := txm.Commit(txn.StageIndex, mustJSON(indexes)); err != nil {
		return 0, err
	}
	return mismatches, nil
}

// recoverCorruptIndex damages a committed artifact, runs recovery, and
// rebuilds the rolled-back stage. It proves a stage whose checksum fails is
// never reused.
func recoverCorruptIndex(txm *txn.Manager, container model.Container, opt DemoOptions, res *Result) (int, error) {
	path := txm.ArtifactPath(txn.StageIndex)
	if err := os.WriteFile(path, []byte(`{"corrupted":true}`), 0o644); err != nil {
		return 0, model.Wrap(model.KindRecovery, "pipeline", "corrupt index artifact", err)
	}
	res.InjectedFaults++
	// The log has grown since Open; refresh the manager's view so recovery
	// judges the artifact that is actually on disk now.
	if err := txm.Rescan(); err != nil {
		return 0, model.Wrap(model.KindRecovery, "pipeline", "rescan transaction log", err)
	}
	stage, err := txm.Recover()
	if err != nil {
		return 0, err
	}
	res.RollbackStage = txm.RollbackStage()
	if stage != "" {
		res.ResumeStage = string(stage)
	}
	res.Notes = append(res.Notes,
		"committed index artifact failed its checksum; recovery rolled back to the last atomic boundary")
	return buildIndexes(txm, container, opt, res)
}

func hasKind(list []fixture.FaultKind, k fixture.FaultKind) bool {
	for _, v := range list {
		if v == k {
			return true
		}
	}
	return false
}

func has(m map[txn.Stage]txn.Checkpoint, s txn.Stage) bool {
	_, ok := m[s]
	return ok
}

func mustJSON(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func validStage(s txn.Stage) bool { return stageIndex(s) >= 0 }

func stageIndex(s txn.Stage) int {
	for i, v := range txn.Order {
		if v == s {
			return i
		}
	}
	return -1
}

func probeAll(mp4Path, mxfPath string, out *model.Container, res *Result) error {
	cm, err := probe.MP4(mp4Path, probe.DefaultLimits())
	if err != nil {
		return err
	}
	cx, err := probe.MXF(mxfPath)
	if err != nil {
		return err
	}
	out.Tracks = append(out.Tracks, cm.Tracks...)
	out.Tracks = append(out.Tracks, cx.Tracks...)
	out.Format = "mxf+mp4"
	results, err := normalize.Container(out)
	if err != nil {
		return err
	}
	for _, n := range results {
		for _, note := range n.Notes {
			res.Notes = append(res.Notes, fmt.Sprintf("track %d: %s", n.TrackID, note))
		}
	}
	return nil
}

func runRTP(opt DemoOptions, res *Result) (rtp.LossReport, rtp.Stats, int, error) {
	const clockRate = 90000
	const samples = 120
	const payloadBytes = 800
	pkts := fixture.Packetise(opt.Seed, 1, samples, payloadBytes, clockRate, clockRate/30, 65530, 0)
	params := fixture.FaultParams{
		DuplicateAt: []int{7, 23, 41},
		ReorderAt:   []int{12, 55, 77},
		BurstStart:  90,
		BurstLen:    4, // short: bridged from intact neighbours
		Burst2Start: 100,
		Burst2Len:   12, // long: reported as unrecoverable
	}
	arrivals, faults := fixture.ApplyFaults(pkts, opt.Faults, params)
	cfg := rtp.DefaultConfig()
	cfg.ClockRate = clockRate
	cfg.InitialWindow = 32
	cfg.MinWindow = 8
	cfg.MaxWindow = 256
	recv, err := rtp.NewReceiver(cfg)
	if err != nil {
		return rtp.LossReport{}, rtp.Stats{}, 0, err
	}
	var delivered []rtp.Packet
	for i, p := range arrivals {
		if n := len(opt.BandwidthMbps); n > 0 {
			recv.SetBandwidth(opt.BandwidthMbps[i*n/len(arrivals)%n])
		}
		got, err := recv.Push(p)
		if err != nil {
			return rtp.LossReport{}, rtp.Stats{}, 0, err
		}
		delivered = append(delivered, got...)
	}
	delivered = append(delivered, recv.Flush()...)
	stats := recv.Stats()
	rep := rtp.ClassifyLoss(stats, delivered, opt.MaxBurst)
	tr, err := recv.Track(1, delivered)
	if err != nil {
		return rtp.LossReport{}, rtp.Stats{}, 0, err
	}
	if err := rtp.DamagedRanges(&tr, rep); err != nil {
		return rtp.LossReport{}, rtp.Stats{}, 0, err
	}
	if len(rep.Gaps) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"rtp: %d gap(s), %d recovered packet(s), %d unrecoverable",
			len(rep.Gaps), rep.RecoveredSamples, rep.UnrecoverableSamples))
	}
	if n := len(stats.WindowShifts); n > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("rtp: reorder window adapted %d time(s) to bandwidth", n))
	}
	return rep, stats, faults, nil
}

// targetSpecs is the delivery manifest, authored from the spec rather than
// from probed media. Targets carry the spec's 2% bitrate safety margin.
func targetSpecs() []manifest.TrackTarget {
	spec := func(id uint32, frames, delta, timescale int64, sampleBytes float64) manifest.TrackTarget {
		dur, _ := clock.New(frames*delta, timescale)
		// A sample carries sampleBytes and one frame lasts delta/timescale
		// seconds, so the rate is sampleBytes*8*timescale/delta.
		rate := sampleBytes * 8 * float64(timescale) / float64(delta) / 1000
		return manifest.TrackTarget{
			TrackID: id, Start: clock.Zero(), End: dur,
			TargetBitrateKbps: rate / (1 - headroomFraction),
		}
	}
	return []manifest.TrackTarget{
		spec(1, videoFrames, videoDelta, videoTimescale, videoBytes),
		spec(2, audioFrames, audioDelta, audioTimescale, audioBytes),
		spec(3, videoFrames, videoDelta, videoTimescale, mxfEssenceBytes),
	}
}

func writeFixtures(seed int64, mp4Path, mxfPath string) error {
	mp4 := fixture.MP4Spec{Tracks: []fixture.TrackSpec{
		{
			ID: 1, Kind: "video", Codec: "avc1", Timescale: videoTimescale,
			Frames: videoFrames, FrameDelta: videoDelta,
			Width: 1920, Height: 1080,
			Rotation: 90, CropLeft: 4, CropRight: 4, SampleBytes: videoBytes, GOPLength: 30,
			Color: &fixture.ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
		},
		{
			ID: 2, Kind: "audio", Codec: "mp4a", Timescale: audioTimescale,
			Frames: audioFrames, FrameDelta: audioDelta, SampleBytes: audioBytes,
		},
	}}
	if _, err := fixture.WriteMP4(mp4Path, mp4, seed); err != nil {
		return model.Wrap(model.KindInternal, "pipeline", "write mp4 fixture", err)
	}
	mxf := fixture.MXFSpec{Name: "studio camera", Tracks: []fixture.MXFTrack{
		{
			// The camera is track 3 in the unified timeline: MP4 video is 1
			// and MP4 audio is 2.
			TrackID: 3, Video: true, EditRateN: videoTimescale, EditRateD: videoDelta,
			EditUnits: videoFrames, StoredWidth: 1920, StoredHeight: 1080,
			Rotation: 270, HorizCrop: 4, EssenceBytes: mxfEssenceBytes,
			Color: &fixture.ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
		},
	}}
	if _, err := fixture.WriteMXF(mxfPath, mxf, seed); err != nil {
		return model.Wrap(model.KindInternal, "pipeline", "write mxf fixture", err)
	}
	return nil
}

// measureQuality compares delivered sample bytes against the deterministic
// reference the fixture generator produced. Nothing is decoded: the metric is
// defined on the seeded payload pattern, so it is exactly reproducible.
func measureQuality(seed int64, c model.Container, paths map[string]string) (report.Quality, error) {
	q := report.Quality{Method: "deterministic seeded payload (no frame decode)"}
	files := map[string]*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	var total float64
	var measured int
	for _, t := range c.Tracks {
		if len(t.Samples) == 0 {
			continue
		}
		path, ok := paths[t.Source]
		if !ok {
			continue
		}
		f := files[path]
		if f == nil {
			var err error
			if f, err = os.Open(path); err != nil {
				return q, model.Wrap(model.KindInternal, "pipeline", "open media for quality", err)
			}
			files[path] = f
		}
		for _, s := range t.Samples {
			if s.Damaged || s.Size == 0 {
				continue
			}
			got := make([]byte, s.Size)
			if _, err := f.ReadAt(got, s.Offset); err != nil && err != io.EOF {
				return q, model.Wrap(model.KindInvalidMedia, "pipeline",
					fmt.Sprintf("read sample %d of track %d", s.Index, t.ID), err)
			}
			want := fixture.SampleBytes(seed, t.ID, s.Index, int(s.Size))
			psnr, err := report.DeterministicPSNR(want, got)
			if err != nil {
				return q, err
			}
			total += psnr
			measured++
		}
	}
	q.Measured = measured
	if measured > 0 {
		q.PSNRdB = total / float64(measured)
	}
	return q, nil
}
