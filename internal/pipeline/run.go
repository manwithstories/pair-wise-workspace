// Package pipeline wires the archivecheck subsystems into a single run.  It
// generates a sealed source, parses it, repairs the timeline from transport
// recovery, rebuilds the index and verifies the manifest, committing every
// stage through the transactional layer.  A run is a pure function of its
// configuration, so an interrupted run resumes without recounting work.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"archivecheck/internal/ebml"
	"archivecheck/internal/fault"
	"archivecheck/internal/fixture"
	"archivecheck/internal/index"
	"archivecheck/internal/manifest"
	"archivecheck/internal/transport"
	"archivecheck/internal/txn"
	"archivecheck/internal/verify"
)

// Config is the complete deterministic input to a run.
type Config struct {
	Seed          int64
	FrameCount    int
	LossBurst     int
	Reorder       int
	Duplicate     int
	BandwidthMbps []float64
	// Corrupt requests the damaged sample (truncation, timestamp regress,
	// duplicate timestamps, unknown-size masters).
	Corrupt bool
	// InterruptAfter names the stage after which the run aborts, simulating
	// an interruption that leaves a resumable journal.
	InterruptAfter string
	// WorkingDir is the run directory; source, journal, artifacts and spill
	// files live in isolated subdirectories beneath it.
	WorkingDir string
	// MemoryBudget is the transport buffer budget in bytes; 0 selects 1 MiB.
	MemoryBudget int64
	// Resume continues from an existing journal instead of a fresh run.
	Resume bool
	// ManifestSkewMS shifts the archive's declared track boundaries by this
	// many milliseconds, simulating a checklist whose timeline has drifted
	// from the media.  A skew beyond 1 ms must be rejected at commit.
	ManifestSkewMS float64
	// CrashOnInterrupt simulates an abrupt process death: the run stops after
	// InterruptAfter without committing the next stage and leaves any pending
	// temporary artifact and journal tail in place for recovery to clean up.
	CrashOnInterrupt bool
}

// Report is everything the command prints.
type Report struct {
	Stats        verify.Stats         `json:"stats"`
	Faults       []*fault.Fault       `json:"faults"`
	ExitCode     int                  `json:"exit_code"`
	Stages       []string             `json:"committed_stages"`
	Resumed      []string             `json:"resumed_stages"`
	Rollbacks    int                  `json:"rollback_count"`
	IndexPath    string               `json:"index_path"`
	ManifestPath string               `json:"manifest_path"`
	SidecarPath  string               `json:"sidecar_path"`
	Recovered    bool                 `json:"recovered"`
	Truncated    bool                 `json:"source_truncated"`
	Blocks       int                  `json:"blocks"`
	Spill        transport.SpillStats `json:"spill"`
	Determinism  string               `json:"determinism_signature"`
	ResumeDigest string               `json:"resume_index_sha256,omitempty"`
}

// Tracks are the fixed generated tracks: 25 fps video, 25 fps audio in 48 kHz
// units, and sparse subtitles sharing the video frame clock.
func Tracks() []fixture.TrackSpec {
	return []fixture.TrackSpec{
		{Number: 1, Kind: ebml.TrackVideo, Codec: "V_VP8", Timescale: 1000, FrameDur: 40, PayloadSize: 96},
		{Number: 2, Kind: ebml.TrackAudio, Codec: "A_PCM/INT/LIT", Timescale: 48000, FrameDur: 1920},
		{Number: 3, Kind: ebml.TrackSubtitle, Codec: "S_TEXT/UTF8", Timescale: 1000, FrameDur: 200},
	}
}

const videoTrack = uint64(1)

// Run executes the pipeline once.  Infrastructure failures return an error;
// diagnosed input damage is folded into the fault set and exit code.
func Run(cfg Config) (*Report, error) {
	cfg = cfg.withDefaults()
	rep := &Report{ExitCode: fault.ExitOK}
	dirs, err := prepare(cfg)
	if err != nil {
		return nil, err
	}

	plan, wire, err := generate(cfg)
	if err != nil {
		return nil, err
	}
	rep.Truncated = plan.Truncated
	if err := writeIfChanged(dirs.source, plan.Data); err != nil {
		return nil, err
	}

	fs := &fault.Set{}
	mgr, adopted, err := txn.New(txn.Config{Dir: dirs.state, OutDir: dirs.out, Faults: fs})
	if err != nil {
		return nil, err
	}
	defer mgr.Close()
	rep.Resumed = stageNames(adopted)
	rep.Recovered = len(adopted) > 0
	if mgr.Rollbacks() > 0 {
		fs.Add(fault.New(fault.ModuleTxn, fault.KindInterrupted, fault.NoOffset(),
			fmt.Sprintf("recovery rolled back %d uncommitted stage(s)", mgr.Rollbacks())).AsRecovery())
	}

	// Transport: dedupe/reorder the RTP arrival stream.
	wireFaults := &fault.Set{}
	res := transport.NewReorderer(transport.Jitter(2*(cfg.Reorder+1))).Process(wire.Packets, wireFaults)

	// Bandwidth-adaptive buffer, spilling beyond the budget.
	spill, err := runBuffer(cfg, dirs.spill, wire, res, wireFaults)
	if err != nil {
		return nil, err
	}
	rep.Spill = spill

	// Parse: EBML structure and blocks from the sealed source.
	header, blocks, err := parseSource(dirs.source, plan, fs)
	if err != nil {
		return nil, err
	}
	rep.Blocks = len(blocks)
	if err := commitParse(mgr, adopted, rep, header, blocks); err != nil {
		return rep, err
	}
	if hit, err := maybeInterrupt(cfg, dirs, rep, fs, mgr, txn.StageParse); hit {
		return rep, err
	}

	// Timeline repair, carrying transport loss into the frame list.
	tl := buildTimeline(header, blocks, res, wire)
	tlResult := tl.Repair()

	// Carry transport and timeline diagnoses into the run's fault set so the
	// report locates damage across modules.
	fs.AddAll(wireFaults)
	fs.AddAll(tlResult.Faults)
	if err := commitStage(mgr, adopted, txn.StageRepair, func() error {
		return verifyRepair(tlResult)
	}); err != nil {
		return rep, err
	}
	if hit, err := maybeInterrupt(cfg, dirs, rep, fs, mgr, txn.StageRepair); hit {
		return rep, err
	}

	// Index: random access over repaired keyframes, verified against source.
	verifier := &fileVerifier{path: dirs.source, size: int64(len(plan.Data))}
	idx, err := index.Build(tlResult.Tracks, header.TrackOrder, index.BuildConfig{KeyframesOnly: true}, verifier, fs)
	if err != nil {
		return nil, err
	}
	if err := storeIndex(mgr, txn.StageIndex, dirs.out, idx); err != nil {
		return rep, err
	}
	if hit, err := maybeInterrupt(cfg, dirs, rep, fs, mgr, txn.StageIndex); hit {
		return rep, err
	}

	// Manifest: generate then verify within 1 ms before commit.
	declared := declaredExtents(plan, header)
	if cfg.ManifestSkewMS != 0 {
		declared = skewDeclared(declared, cfg.ManifestSkewMS)
	}
	doc := manifest.Generate(tlResult.Tracks, declared, idx.Digest)
	align := manifest.Verify(doc, tlResult.Tracks)
	if err := storeManifest(mgr, txn.StageManifest, dirs.out, doc, align.OK); err != nil {
		// A verification failure is diagnosed damage: record its faults and
		// refuse the commit with exit 2.  Infrastructure errors propagate.
		if !align.OK {
			fs.AddAll(align.Faults)
			quality := qualityStats(wire, res)
			rep.Stats = verify.Stats{
				Quality:          quality,
				Bitrate:          computeBitrate(plan, tlResult),
				Tracks:           verify.Tracks(tlResult.Tracks, header.TrackOrder),
				LostPackets:      res.Lost,
				DuplicatePackets: res.Duplicates,
				ReorderedPackets: res.Reordered,
				TimestampRepairs: len(tlResult.Repairs),
				IndexEntries:     len(idx.Entries),
				IndexSHA256:      idx.Digest,
				ManifestAlignMS:  align.MaxErrorMs,
				RollbackCount:    mgr.Rollbacks(),
				PacketLossRate:   res.LossRate(),
			}
			rep.Rollbacks = mgr.Rollbacks()
			rep.Faults = fs.Faults()
			rep.ExitCode = fs.ExitCode()
			// Reflect what actually committed: parse and repair are committed by
			// this point; the index was rolled back when the manifest failed.
			rep.Stages = appendUnique(rep.Stages, string(txn.StageParse), string(txn.StageRepair))
			return rep, nil
		}
		return rep, err
	}

	quality := qualityStats(wire, res)
	bitrate := computeBitrate(plan, tlResult)
	rep.Stats = verify.Stats{
		Quality:          quality,
		Bitrate:          bitrate,
		Tracks:           verify.Tracks(tlResult.Tracks, header.TrackOrder),
		LostPackets:      res.Lost,
		DuplicatePackets: res.Duplicates,
		ReorderedPackets: res.Reordered,
		TimestampRepairs: len(tlResult.Repairs),
		IndexEntries:     len(idx.Entries),
		IndexSHA256:      idx.Digest,
		ManifestAlignMS:  align.MaxErrorMs,
		RollbackCount:    mgr.Rollbacks(),
		PacketLossRate:   res.LossRate(),
	}
	rep.IndexPath = filepath.Join(dirs.out, "index.json")
	rep.ManifestPath = filepath.Join(dirs.out, "manifest.json")
	rep.SidecarPath = filepath.Join(dirs.out, "index.mp4")
	rep.Stages = appendUnique(rep.Stages, string(txn.StageIndex), string(txn.StageManifest))
	rep.Determinism = rep.Stats.Signature()
	rep.Rollbacks = mgr.Rollbacks()
	rep.Faults = fs.Faults()
	rep.ExitCode = fs.ExitCode()

	// A run whose bitrate or quality target fails is diagnosed damage.
	if !bitrate.WithinTarget && rep.ExitCode == fault.ExitOK {
		rep.ExitCode = fault.ExitDamaged
	}
	if quality.Samples == 0 && rep.ExitCode == fault.ExitOK {
		rep.ExitCode = fault.ExitDamaged
	}
	return rep, nil
}

func (c Config) withDefaults() Config {
	if c.FrameCount <= 0 {
		c.FrameCount = 60
	}
	if c.WorkingDir == "" {
		c.WorkingDir = "."
	}
	if c.MemoryBudget <= 0 {
		c.MemoryBudget = 1 << 20
	}
	return c
}

type runDirs struct {
	source, state, out, spill string
}

func prepare(cfg Config) (runDirs, error) {
	d := runDirs{
		source: filepath.Join(cfg.WorkingDir, "source", "master.mkv"),
		state:  filepath.Join(cfg.WorkingDir, "state"),
		out:    filepath.Join(cfg.WorkingDir, "out"),
		spill:  filepath.Join(cfg.WorkingDir, "spill"),
	}
	for _, dir := range []string{filepath.Dir(d.source), d.state, d.out, d.spill} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return d, err
		}
	}
	return d, nil
}

// maybeInterrupt stops the run after stage.  With CrashOnInterrupt a partial
// artifact and a torn journal record are left behind, so the next run must
// roll them back; otherwise the interruption is clean and only the journal
// state carries over.  It reports whether the run stopped.
func maybeInterrupt(cfg Config, dirs runDirs, rep *Report, fs *fault.Set, mgr *txn.Manager, stage txn.Stage) (bool, error) {
	if cfg.InterruptAfter != string(stage) {
		return false, nil
	}
	fs.Add(fault.New(fault.ModuleTxn, fault.KindInterrupted, fault.NoOffset(),
		fmt.Sprintf("run interrupted after committed stage %q; rerun with --resume to continue", stage)))
	if cfg.CrashOnInterrupt {
		// Leave a dangling temporary artifact from the next stage as an
		// interrupted writer would.
		tmp := filepath.Join(dirs.out, ".tmp", string(nextStage(stage))+"-crash.part")
		_ = os.MkdirAll(filepath.Dir(tmp), 0o755)
		_ = os.WriteFile(tmp, []byte("partial artifact from interrupted writer"), 0o644)
		if err := mgr.CrashAfter(stage); err != nil {
			return true, err
		}
	}
	rep.Faults = fs.Faults()
	rep.ExitCode = fs.ExitCode()
	rep.Rollbacks = mgr.Rollbacks()
	rep.Stats.RollbackCount = mgr.Rollbacks()
	rep.Stages = appendUnique(rep.Stages, string(stage))
	return true, nil
}

func nextStage(s txn.Stage) txn.Stage {
	for i, o := range txn.Order {
		if o == s && i+1 < len(txn.Order) {
			return txn.Order[i+1]
		}
	}
	return s
}

func stageNames(m map[txn.Stage]txn.Artifact) []string {
	out := make([]string, 0, len(m))
	for _, s := range txn.Order {
		if _, ok := m[s]; ok {
			out = append(out, string(s))
		}
	}
	return out
}

func writeIfChanged(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}

// appendUnique appends stage names that are not already present, keeping the
// committed-stage list free of duplicates regardless of path.
func appendUnique(list []string, names ...string) []string {
	seen := make(map[string]bool, len(list)+len(names))
	for _, n := range list {
		seen[n] = true
	}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			list = append(list, n)
		}
	}
	return list
}
