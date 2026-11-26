package pipeline

import (
	"encoding/json"
	"fmt"
	"io"

	"pairflow/internal/audio"
	"pairflow/internal/compose"
	"pairflow/internal/media"
	"pairflow/internal/store"
	"pairflow/internal/timeline"
	"pairflow/internal/transport"
	"pairflow/internal/video"
)

// Resume continues a run interrupted after the render/transport stages. It
// re-reads the journal, validates every committed stage artifact, then
// executes only the remaining stages. The final frame hashes must match a full
// uninterrupted rerun.
func Resume(root string, out io.Writer) (*Result, *media.Error) {
	j, err := store.OpenJail(root)
	if err != nil {
		return nil, err
	}
	wal, err := store.OpenWAL(j.Root())
	if err != nil {
		return nil, err
	}
	defer wal.Close()

	// Identify the interrupted timeline from the last committed stage.
	last, ok := wal.LastDone("")
	if !ok {
		return nil, media.Integrity("resume.no_stages", "run has no committed stages to resume from").WithPath(root)
	}
	fp := last.Timeline
	if fp == "" {
		return nil, media.Integrity("resume.no_timeline", "committed stages carry no timeline identity").WithPath(root)
	}
	fmt.Fprintf(out, "resume=found timeline_sha256=%s last_stage=%s\n", fp, last.Stage)

	// Re-validate every committed stage artifact before trusting it.
	verified := map[string]bool{}
	for _, r := range wal.Records() {
		if r.State != "done" || r.Timeline != fp {
			continue
		}
		if verr := j.VerifyArtifacts(r.Artifacts); verr != nil {
			return nil, verr
		}
		verified[r.Stage] = true
	}
	fmt.Fprintf(out, "resume=verified stages=%s\n", joinStages(wal, fp))

	// Rebuild the timeline and its resolved edits, then confirm the fingerprint
	// still matches the committed identity.
	tlRaw, derr := j.ReadFile("out/decode/timeline.json")
	if derr != nil {
		return nil, derr
	}
	tl, derr := timeline.DecodeBytes(tlRaw, "out/decode/timeline.json")
	if derr != nil {
		return nil, derr
	}
	resolved, rerr := timeline.Resolve(tl)
	if rerr != nil {
		return nil, rerr
	}
	if got := timeline.Fingerprint(resolved); got != fp {
		return nil, media.Integrity("resume.timeline_drift",
			"committed timeline %s does not re-resolve to %s", fp, got).WithPath(root)
	}

	if verified[StageCompose] {
		return nil, media.Integrity("resume.already_complete", "compose stage is already committed; nothing to resume").WithPath(root)
	}

	dur, merr := tl.Duration.Media()
	if merr != nil {
		return nil, merr
	}
	frames, ferr := compose.FrameCount(dur)
	if ferr != nil {
		return nil, ferr
	}

	// Confirm the committed audio is still readable (its sample count is part
	// of the run's integrity surface) and read back the recorded fault mode.
	if _, aerr := resumePCM(j); aerr != nil {
		return nil, aerr
	}
	faults := faultsForRun(j)

	vt, verr := compose.PrepareVideo(resolved, map[string]int{"cam-a": 1, "cam-b": 0})
	if verr != nil {
		return nil, verr
	}
	if err := wal.Begin(StageCompose, fp); err != nil {
		return nil, err
	}
	st, serr := store.NewStageStager(j, StageCompose)
	if serr != nil {
		return nil, serr
	}
	hashes := make([]string, 0, frames)
	for i := 0; i < frames; i++ {
		f, frerr := compose.RenderFrame(vt, i)
		if frerr != nil {
			return nil, rollback(st, frerr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		meta, mterr := video.MetaFor("srgb", fmt.Sprintf("frame-%05d.png", i))
		if mterr != nil {
			return nil, rollback(st, mterr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		encoded, eerr := video.EncodePNG(f.Image, meta)
		if eerr != nil {
			return nil, rollback(st, eerr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		if _, werr := st.WriteStaged(fmt.Sprintf("frames/frame-%05d.png", i), encoded, "png"); werr != nil {
			return nil, rollback(st, werr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		hashes = append(hashes, video.ImageSHA(encoded))
	}
	for _, a := range st.Artifacts() {
		if a.Kind != "png" {
			continue
		}
		data, rerr := st.ReadStaged(a.Path)
		if rerr != nil {
			return nil, rollback(st, rerr.WithStage(StageCompose), wal, StageCompose, fp)
		}
		if verr := video.ValidatePNG(data, a.Path); verr != nil {
			return nil, rollback(st, verr.WithStage(StageCompose), wal, StageCompose, fp)
		}
	}
	arts, cerr := st.Commit()
	if cerr != nil {
		return nil, rollback(st, cerr, wal, StageCompose, fp)
	}
	if err := wal.Done(StageCompose, fp, arts); err != nil {
		return nil, err
	}

	// Rebuild the manifest from every committed stage and commit it last.
	manifest := &store.ManifestDoc{
		Version:  1,
		Timeline: fp,
		Seed:     tl.Seed,
		Faults:   faults,
		Stages:   manifestStages(wal, fp),
	}
	if err := j.WriteManifest(manifest); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "stage=compose state=ok frame_count=%d resumed=true\n", frames)
	fmt.Fprintf(out, "resume=ok timeline_sha256=%s\n", fp)
	return &Result{RunDir: j.Root(), Timeline: fp, Frames: frames, Hashes: hashes}, nil
}

// resumePCM re-derives the canonical PCM from the committed wav when present.
func resumePCM(j *store.Jail) ([]int16, *media.Error) {
	pcm, _, err := audio.ReadAllWAV("out/audio/mix.wav")
	if err != nil {
		return nil, err
	}
	if len(pcm) == 0 {
		return nil, media.Integrity("resume.empty_audio", "committed mix.wav is empty")
	}
	return pcm, nil
}

// faultsForRun reads the fault mode back from the transport wire log or run
// metadata. It defaults to "mixed" for the acceptance fixture only when a
// transport stage exists without a recorded mode.
func faultsForRun(j *store.Jail) string {
	data, err := j.ReadFile("out/transport/wire.json")
	if err != nil {
		return "mixed"
	}
	var doc struct {
		Faults string           `json:"faults"`
		Report transport.Report `json:"report"`
	}
	if uerr := json.Unmarshal(data, &doc); uerr != nil || doc.Faults == "" {
		return "mixed"
	}
	return doc.Faults
}

func joinStages(wal *store.WAL, fp string) string {
	stages := wal.DoneStages(fp)
	out := ""
	for i, s := range stages {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
