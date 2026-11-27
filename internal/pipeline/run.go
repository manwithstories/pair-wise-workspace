package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"mediad/internal/index"
	"mediad/internal/model"
	"mediad/internal/output"
	"mediad/internal/sync"
	"mediad/internal/transport"
)

// Config controls one delivery run.
type Config struct {
	InputDir        string
	OutDir          string
	FaultProfile    string
	StopAfterSamples int // >0 requests an interrupt (exit 75) after N samples
	Resume          bool
}

// Result is the structured outcome reported to the CLI.
type Result struct {
	Manifest     *output.Manifest
	ResumeMS     int64
	Interrupted  bool
	InputSHA     string
	OutputPath   string
}

// Run executes the streaming pipeline for every source file in InputDir.
func Run(cfg Config) (*Result, error) {
	files, err := listSources(cfg.InputDir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, model.NewFault(model.FaultMissingIndex, model.OffsetUnknown,
			cfg.InputDir, "no MP4/MOV sources found")
	}
	store, err := index.NewStore(cfg.OutDir)
	if err != nil {
		return nil, err
	}
	cpStore, err := NewCheckpointStore(cfg.OutDir)
	if err != nil {
		return nil, err
	}

	// The primary source drives the headline manifest fields.
	primary := files[0]
	scanRes, err := store.GetOrScan(primary)
	if err != nil {
		return nil, err
	}
	idx := scanRes.Index
	manifest := output.NewManifest(idx.SHA256)
	manifest.RecordFaults(idx.Faults)

	// checkpoint validation: a changed source invalidates any prior run
	start := time.Now()
	cp, err := cpStore.Load()
	if err != nil {
		return nil, err
	}
	resumed := false
	if cp != nil {
		if err := cp.Validate(idx.SHA256); err != nil {
			// stale checkpoint: discard partial output and restart cleanly
			_ = cpStore.Clear()
			cp = nil
		} else {
			resumed = true
		}
	}

	out, err := processOne(cfg, idx, cp, manifest)
	resumeMS := time.Since(start).Milliseconds()
	if err != nil && !errors.Is(err, model.ErrInterrupted) {
		return nil, err
	}
	manifest.Resumed = resumed
	out.ResumeMS = resumeMS
	return out, err
}

// processOne handles one file end to end and mutates the manifest.
func processOne(cfg Config, idx *index.FileIndex, cp *Checkpoint,
	manifest *output.Manifest) (*Result, error) {
	// 1) packetize all tracks, then apply deterministic impairment
	var originals []*transport.Packet
	for _, tr := range idx.Tracks {
		originals = append(originals, transport.PacketizeTrack(tr)...)
	}
	profile, err := transport.LoadFaultProfile(cfg.FaultProfile)
	if err != nil {
		return nil, err
	}
	impaired := transport.NewInjector(profile).Inject(originals)

	// 2) bounded jitter + NACK + XOR recovery
	delivered, gaps, stats, mechanism := transport.Run(originals, impaired,
		transport.DefaultJitterConfig())
	decisions := buildDecisions(originals, delivered, gaps, mechanism)
	manifest.RecordTransport(stats, gaps, decisions)

	// 3) shared-clock alignment; drift and faults propagate to provenance
	align := sync.NewAligner().Align(idx, gaps)
	manifest.AVDriftMS = align.AudioVideoDriftMS
	manifest.RecordFaults(align.Faults)

	// 4) remux the original sample bytes (no transcoding). A sample is
	// included only when at least one of its RTP packets survived or was
	// recovered; permanently missing samples are skipped and stay gaps.
	presentSamples := presentSampleSet(delivered)
	reader, err := NewSampleReader(idx.Path, idx)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	blockDir := filepath.Join(cfg.OutDir, ".mediad", "blocks")
	remuxer, err := output.NewRemuxer(blockDir, idx.Name)
	if err != nil {
		return nil, err
	}
	if cp != nil && cfg.Resume {
		if err := remuxer.DiscardPartials(); err != nil {
			return nil, err
		}
		if err := remuxer.Resume(toBlockChecksums(cp.Blocks)); err != nil {
			return nil, err
		}
	}
	samplesDone, interrupted, err := streamSamples(remuxer, reader, idx,
		presentSamples, cfg, cp)
	if cerr := remuxer.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}

	// 5) persist checkpoint (interrupted) or assemble the final deliverable
	cpStore, _ := NewCheckpointStore(cfg.OutDir)
	if interrupted {
		newCP := &Checkpoint{
			InputSHA256:      idx.SHA256,
			OutputName:       idx.Name,
			SamplesDone:      samplesDone,
			RecoveredCount:   stats.RecoveredPackets,
			UnrecoveredCount: stats.UnrecoveredPackets,
			RunningHash:      remuxer.RunningDigest(),
			Blocks:           fromOutputBlocks(remuxer.CommittedBlocks()),
			Interrupted:      true,
		}
		if err := cpStore.Save(newCP); err != nil {
			return nil, err
		}
		return &Result{Manifest: manifest, Interrupted: true,
			InputSHA: idx.SHA256}, model.ErrInterrupted
	}

	finalPath := filepath.Join(cfg.OutDir, "deliverable-"+idx.Name)
	digest, n, err := output.ConcatBlocks(blockDir, idx.Name, finalPath,
		remuxer.BlockCount())
	if err != nil {
		return nil, err
	}
	manifest.OutputSHA256 = digest
	manifest.OutputBytes = n
	manifest.Checkpoint = output.CheckpointRef{
		SamplesDone: samplesDone, RunningHash: remuxer.RunningDigest(),
		Blocks: remuxer.CommittedBlocks(), Interrupted: false}
	if err := manifest.Write(cfg.OutDir); err != nil {
		return nil, err
	}
	_ = cpStore.Clear()
	return &Result{Manifest: manifest, InputSHA: idx.SHA256,
		OutputPath: finalPath}, nil
}

// presentSampleSet returns the (track, sample) pairs whose media survived
// the transport (direct, NACK or XOR). Fragments collapse to one entry.
func presentSampleSet(delivered []*transport.Packet) map[sampleKey]bool {
	out := make(map[sampleKey]bool)
	for _, p := range delivered {
		out[sampleKey{track: uint32(p.TrackIdx), index: p.SampleIdx}] = true
	}
	return out
}

// streamSamples copies original on-disk sample bytes into the remuxer in
// deterministic (track, sample-index) order, honoring stop-after and a prior
// checkpoint so resumed runs never re-emit already-committed samples.
func streamSamples(remuxer *output.Remuxer, reader *SampleReader,
	idx *index.FileIndex, present map[sampleKey]bool, cfg Config,
	cp *Checkpoint) (int, bool, error) {
	skip := 0
	if cp != nil && cfg.Resume {
		skip = cp.SamplesDone
	}
	written := 0
	for ti, tr := range idx.Tracks {
		for n := uint32(0); n < uint32(len(tr.Samples)); n++ {
			if !present[sampleKey{track: uint32(ti), index: n}] {
				continue // unrecovered sample: timeline gap, nothing to remux
			}
			if written < skip {
				written++
				continue
			}
			buf, err := reader.ReadSample(uint32(ti), n)
			if err != nil {
				return written, false, err
			}
			if len(buf) == 0 {
				continue
			}
			if _, err := remuxer.Write(buf); err != nil {
				return written, false, err
			}
			written++
			if cfg.StopAfterSamples > 0 && written-skip >= cfg.StopAfterSamples {
				return written, true, nil
			}
		}
	}
	return written, false, nil
}

// toBlockChecksums converts checkpoint block records to output records.
func toBlockChecksums(in []BlockState) []output.BlockChecksum {
	out := make([]output.BlockChecksum, len(in))
	for i, b := range in {
		out[i] = output.BlockChecksum{Index: b.Index, ByteLength: b.ByteLength, SHA256: b.SHA256}
	}
	return out
}

// fromOutputBlocks converts output block records into checkpoint state.
func fromOutputBlocks(in []output.BlockChecksum) []BlockState {
	out := make([]BlockState, len(in))
	for i, b := range in {
		out[i] = BlockState{Index: b.Index, ByteLength: b.ByteLength, SHA256: b.SHA256}
	}
	return out
}

// buildDecisions turns the delivered/missing sets into per-packet decisions.
// Packets that arrived normally are marked "direct"; only repaired packets
// carry nack/xor, and permanently missing ones carry "none".
func buildDecisions(originals, delivered []*transport.Packet,
	gaps []transport.Gap, mechanism map[uint16]string) []output.RecoveryDecision {
	present := make(map[uint16]bool)
	for _, p := range delivered {
		present[p.RTP.SequenceNumber] = true
	}
	out := make([]output.RecoveryDecision, 0, len(originals))
	for _, p := range originals {
		seq := p.RTP.SequenceNumber
		d := output.RecoveryDecision{
			SequenceNumber: seq, PTSNS: int64(p.PTS), Recovered: present[seq],
		}
		if m, ok := mechanism[seq]; ok && present[seq] {
			d.Mechanism = m
		} else if present[seq] {
			d.Mechanism = "direct"
		} else {
			d.Mechanism = "none"
		}
		out = append(out, d)
	}
	return out
}

// listSources returns MP4/MOV files in dir, sorted for stable ordering.
func listSources(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(name)
		if ext == ".mp4" || ext == ".mov" || ext == ".m4a" {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}
