// Package pipeline orchestrates the media stages: decode/resolve, normalize,
// transport recovery, compose, transactional commit and audit.
package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"pairflow/internal/audio"
	"pairflow/internal/compose"
	"pairflow/internal/media"
	"pairflow/internal/store"
	"pairflow/internal/timeline"
	"pairflow/internal/transport"
	"pairflow/internal/video"
)

// Stage names, in execution order.
const (
	StageDecode    = "decode"
	StageAudio     = "audio"
	StageColor     = "color"
	StageTransport = "transport"
	StageCompose   = "compose"
)

// Options configure a run.
type Options struct {
	RunDir         string
	Seed           uint64
	Seconds        int
	Faults         string
	InterruptAfter string // "render" stops after transport, before compose
	BlockFrames    int
	Out            io.Writer // nil collects output for Drain
}

// Run drives the full pipeline and returns the committed result.
func Run(opts Options) (*Result, *media.Error) {
	if opts.BlockFrames <= 0 {
		opts.BlockFrames = 2400 // 50 ms blocks bound working memory
	}
	if err := os.MkdirAll(opts.RunDir, 0o755); err != nil {
		return nil, media.Wrap(err, media.ClassInput, "pipeline.dir", "cannot create run directory").WithPath(opts.RunDir)
	}
	j, err := store.OpenJail(opts.RunDir)
	if err != nil {
		return nil, err
	}
	out := output(opts)
	wal, err := store.OpenWAL(j.Root())
	if err != nil {
		return nil, err
	}
	defer wal.Close()

	// Clear any previously committed outputs so a rerun starts from the
	// requested seed/faults rather than mixing generations.
	if err := clearGenerated(j); err != nil {
		return nil, err
	}

	// Stage 1: decode and resolve. No artifacts yet, but the timeline hash is
	// fixed for every later stage.
	tl, err := timeline.Generate(opts.Seed, opts.Seconds)
	if err != nil {
		return nil, err.WithStage(StageDecode)
	}
	doc, merr := tl.Marshal()
	if merr != nil {
		return nil, merr.WithStage(StageDecode)
	}
	raw, derr := timeline.DecodeBytes(doc, "timeline.json")
	if derr != nil {
		return nil, derr.WithStage(StageDecode)
	}
	resolved, rerr := timeline.Resolve(raw)
	if rerr != nil {
		return nil, rerr.WithStage(StageDecode)
	}
	fp := timeline.Fingerprint(resolved)
	fmt.Fprintf(out, "stage=decode state=ok timeline_sha256=%s tracks=%d\n", fp, len(resolved))
	if err := wal.Begin(StageDecode, fp); err != nil {
		return nil, err
	}
	st, err := store.NewStageStager(j, StageDecode)
	if err != nil {
		return nil, err
	}
	if _, err := st.WriteStaged("timeline.json", doc, "json"); err != nil {
		return nil, err
	}
	arts, err := st.Commit()
	if err != nil {
		return nil, rollback(st, err, wal, StageDecode, fp)
	}
	if err := wal.Done(StageDecode, fp, arts); err != nil {
		return nil, err
	}

	// Stage 2: color normalization and PNG stamping (one metadata artifact).
	if err := prepareColor(resolved); err != nil {
		return nil, err.WithStage(StageColor)
	}
	metaDoc, err := colorDoc(resolved)
	if err != nil {
		return nil, err.WithStage(StageColor)
	}
	fmt.Fprintf(out, "stage=color state=ok sources=%d destination=srgb\n", len(metaDoc.Sources))
	if err := wal.Begin(StageColor, fp); err != nil {
		return nil, err
	}
	st, err = store.NewStageStager(j, StageColor)
	if err != nil {
		return nil, err
	}
	if _, err := st.WriteStaged("color.json", metaDoc.JSON, "json"); err != nil {
		return nil, err
	}
	arts, err = st.Commit()
	if err != nil {
		return nil, rollback(st, err, wal, StageColor, fp)
	}
	if err := wal.Done(StageColor, fp, arts); err != nil {
		return nil, err
	}

	// Stage 3: transport. Render the master mix, packetize it, inject the
	// requested faults and recover what XOR parity allows. The recovered PCM
	// becomes the delivered audio the rest of the pipeline commits.
	at, _, aerr := compose.PrepareAudio(resolved)
	if aerr != nil {
		return nil, aerr.WithStage(StageTransport)
	}
	dur, derr := raw.Duration.Media()
	if derr != nil {
		return nil, derr.WithStage(StageTransport)
	}
	samples, serr := compose.SampleCount(dur)
	if serr != nil {
		return nil, serr.WithStage(StageTransport)
	}
	master, perr := compose.RenderPCM(at, samples, opts.BlockFrames)
	if perr != nil {
		return nil, perr.WithStage(StageTransport)
	}
	tr, terr := transport.Run(master, audio.NormSpec.Channels, opts.Seed, opts.Faults)
	if terr != nil {
		return nil, terr.WithStage(StageTransport)
	}
	wireJSON, jerr := json.MarshalIndent(transportWire{Report: tr.Report, Wire: tr.Wire}, "", "  ")
	if jerr != nil {
		return nil, media.Wrap(jerr, media.ClassInternal, "pipeline.wire_encode", "cannot encode wire log")
	}
	fmt.Fprintf(out, "stage=transport state=ok lost=%d recovered=%d loss_recovery_pct=%.2f\n",
		tr.Report.Lost, tr.Report.Recovered, tr.Report.LossRecoveryPct)
	if err := wal.Begin(StageTransport, fp); err != nil {
		return nil, err
	}
	st, err = store.NewStageStager(j, StageTransport)
	if err != nil {
		return nil, err
	}
	if _, err := st.WriteStaged("wire.json", wireJSON, "wire"); err != nil {
		return nil, err
	}
	arts, err = st.Commit()
	if err != nil {
		return nil, rollback(st, err, wal, StageTransport, fp)
	}
	if err := wal.Done(StageTransport, fp, arts); err != nil {
		return nil, err
	}

	// Stage 4: commit the delivered audio mix.
	pcm := tr.PCM
	if err := audio.ValidateStream(pcm, audio.NormSpec, samples, "mix.wav"); err != nil {
		return nil, err.WithStage(StageAudio)
	}
	silence := audio.SilenceRatio(pcm, audio.NormSpec.Channels)
	wav, err := audio.EncodeWAV(pcm, audio.NormSpec)
	if err != nil {
		return nil, err.WithStage(StageAudio)
	}
	fmt.Fprintf(out, "stage=audio state=ok wav_rate=%d wav_channels=%d pcm_sample_frames=%d gap_silence_pct=%.2f\n",
		audio.NormSpec.Rate, audio.NormSpec.Channels, samples, silence*100)
	if err := wal.Begin(StageAudio, fp); err != nil {
		return nil, err
	}
	st, err = store.NewStageStager(j, StageAudio)
	if err != nil {
		return nil, err
	}
	if _, err := st.WriteStaged("mix.wav", wav, "wav"); err != nil {
		return nil, err
	}
	arts, err = st.Commit()
	if err != nil {
		return nil, rollback(st, err, wal, StageAudio, fp)
	}
	if err := wal.Done(StageAudio, fp, arts); err != nil {
		return nil, err
	}

	// Interruption point: after render/transport, before compose. Durable
	// state lets resume pick up exactly at compose.
	if strings.EqualFold(opts.InterruptAfter, "render") {
		if err := wal.Append(store.Record{Stage: StageTransport, State: "interrupt", Timeline: fp}); err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "stage=transport state=interrupted resume_hint=compose\n")
		return nil, media.Interrupted("pipeline.interrupted",
			"stopped after render; run `mediad resume --run %s` to continue", opts.RunDir)
	}

	// Stage 5: compose frames, hash each one and stage them for commit.
	vt, err := compose.PrepareVideo(resolved, map[string]int{"cam-a": 1, "cam-b": 0})
	if err != nil {
		return nil, err.WithStage(StageCompose)
	}
	frames, err := compose.FrameCount(dur)
	if err != nil {
		return nil, err.WithStage(StageCompose)
	}
	if err := wal.Begin(StageCompose, fp); err != nil {
		return nil, err
	}
	st, err = store.NewStageStager(j, StageCompose)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, frames)
	for i := 0; i < frames; i++ {
		f, ferr := compose.RenderFrame(vt, i)
		if ferr != nil {
			return nil, rollback(st, ferr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		// Every composed frame is sRGB by construction; the source space is
		// recorded per segment in color.json.
		meta, merr := video.MetaFor("srgb", fmt.Sprintf("frame-%05d.png", i))
		if merr != nil {
			return nil, rollback(st, merr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		encoded, eerr := video.EncodePNG(f.Image, meta)
		if eerr != nil {
			return nil, rollback(st, eerr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		name := fmt.Sprintf("frames/frame-%05d.png", i)
		if _, err := st.WriteStaged(name, encoded, "png"); err != nil {
			return nil, rollback(st, err.WithStage(StageCompose), wal, StageCompose, fp)
		}
		hashes = append(hashes, video.ImageSHA(encoded))
	}
	// Fault injection: corrupt a committed-in-staging frame's CRC so the
	// pre-commit validation below must reject and roll back the stage.
	if opts.Faults == transport.ModeBadFrame {
		if _, err := st.CorruptStaged("frames/frame-00030.png"); err != nil {
			return nil, rollback(st, err.WithStage(StageCompose), wal, StageCompose, fp)
		}
	}
	// Validate every staged frame before any commit: a CRC, color or hash
	// failure rolls the entire stage back.
	for i := range st.Artifacts() {
		a := st.Artifacts()[i]
		if a.Kind != "png" {
			continue
		}
		data, err := st.ReadStaged(a.Path)
		if err != nil {
			return nil, rollback(st, err.WithStage(StageCompose), wal, StageCompose, fp)
		}
		if verr := video.ValidatePNG(data, a.Path); verr != nil {
			return nil, rollback(st, verr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		if video.ImageSHA(data) != a.SHA {
			return nil, rollback(st, media.Integrity("compose.frame_hash",
				"staged frame %s hash drifted", a.Path).WithStage(StageCompose), wal, StageCompose, fp)
		}
	}
	arts, err = st.Commit()
	if err != nil {
		return nil, rollback(st, err, wal, StageCompose, fp)
	}
	if err := wal.Done(StageCompose, fp, arts); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "stage=compose state=ok frame_count=%d frame_size=%dx%d fps=%d\n",
		frames, media.FrameWidth, media.FrameHeight, media.VideoFPS)

	// Manifest commits last, atomically, naming every stage artifact.
	manifest := &store.ManifestDoc{
		Version:  1,
		Timeline: fp,
		Seed:     opts.Seed,
		Faults:   opts.Faults,
		Stages:   manifestStages(wal, fp),
	}
	if err := j.WriteManifest(manifest); err != nil {
		return nil, err
	}
	return &Result{
		RunDir:   j.Root(),
		Timeline: fp,
		Frames:   frames,
		Hashes:   hashes,
		Audio:    samples,
		Report:   tr.Report,
	}, nil
}
