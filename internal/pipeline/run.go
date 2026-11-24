package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"mediarig/internal/audio"
	"mediarig/internal/media"
	"mediarig/internal/mp4"
	"mediarig/internal/report"
	"mediarig/internal/txn"
)

// Result is a completed or resumed batch result.
type Result struct {
	Stats        report.Stats
	Manifest     report.Manifest
	ArchivedLine string
}

// RunFresh executes a new batch with injection policy from params.
func RunFresh(params txn.Params) (*Result, error) {
	return execute(params, nil)
}

// Resume loads and verifies WAL state before continuing idempotently.
func Resume(root string) (*Result, error) {
	rec, err := txn.LoadAndVerify(root)
	if err != nil {
		return nil, err
	}
	if rec.Committed {
		if b, err := os.ReadFile(rec.Paths.Stats); err == nil {
			return &Result{ArchivedLine: strings.TrimSpace(string(b))}, nil
		}
	}
	return execute(rec.Params, rec)
}

func execute(params txn.Params, rec *txn.Recovery) (*Result, error) {
	release := txn.AcquireBatch()
	defer release()

	p, err := txn.Layout(params.Root)
	if err != nil {
		return nil, err
	}
	params.Root = p.Root
	if rec != nil {
		p = rec.Paths
	} else {
		if err := p.Ensure(); err != nil {
			return nil, err
		}
		if err := txn.CheckDistinctInputOutput(p.InputFile, p.MP4); err != nil {
			return nil, err
		}
		if err := txn.RejectSymlinkOutput(p.MP4); err != nil {
			return nil, err
		}
		if params.TargetKbps == 0 {
			params.TargetKbps = 1536
		}
		if params.LUFS == 0 {
			params.LUFS = -23
		}
		if params.Rate == 0 {
			params.Rate = 48000
		}
		if params.Layout == "" {
			params.Layout = "stereo"
		}
		if err := txn.WriteJSONAtomic(p.Params, params); err != nil {
			return nil, err
		}
	}
	paramsDigest, err := params.Digest()
	if err != nil {
		return nil, err
	}
	inputDigest, _, err := txn.SHA256File(p.InputFile)
	if err != nil {
		return nil, err
	}
	wal, err := txn.OpenWAL(p.WAL)
	if err != nil {
		return nil, err
	}
	defer wal.Close()

	capRes, err := ReadCapture(p, params)
	if err != nil {
		return nil, err
	}
	captureArtifact, err := writeCaptureArtifact(p, capRes)
	if err != nil {
		return nil, err
	}
	if rec == nil || !rec.StageDone(media.StageCapture) {
		if _, err := wal.AppendCheckpoint(media.StageCapture, inputDigest, paramsDigest,
			[]txn.Artifact{captureArtifact}, "capture and loss policy complete"); err != nil {
			return nil, err
		}
	}

	pcm, loud, err := loadOrBuildAudio(p, params, rec, capRes)
	if err != nil {
		return nil, err
	}
	if rec == nil || !rec.StageDone(media.StageAudio) {
		arts, err := artifactsFor(p.AudioPCM, p.AudioJSON)
		if err != nil {
			return nil, err
		}
		if _, err := wal.AppendCheckpoint(media.StageAudio, inputDigest, paramsDigest, arts, "audio normalized"); err != nil {
			return nil, err
		}
	}
	if rec == nil && strings.EqualFold(params.FailAfter, string(media.StageAudio)) {
		return nil, media.Injected(media.StageAudio, "injected interruption after audio checkpoint")
	}

	color, err := loadOrResolveColor(p, params, rec)
	if err != nil {
		return nil, err
	}
	if rec == nil || !rec.StageDone(media.StageColor) {
		arts, err := artifactsFor(p.ColorJSON)
		if err != nil {
			return nil, err
		}
		if _, err := wal.AppendCheckpoint(media.StageColor, inputDigest, paramsDigest, arts, "color adjudicated"); err != nil {
			return nil, err
		}
	}
	if rec == nil && strings.EqualFold(params.FailAfter, string(media.StageColor)) {
		return nil, media.Injected(media.StageColor, "injected interruption after color checkpoint")
	}

	muxCfg := BuildMuxConfig(pcm, capRes, color)
	validation, err := loadOrMuxMP4(p, rec, wal, inputDigest, paramsDigest, muxCfg)
	if err != nil {
		return nil, err
	}
	if rec == nil && strings.EqualFold(params.FailAfter, string(media.StageMP4)) {
		return nil, media.Injected(media.StageMP4, "injected interruption after MP4 checkpoint")
	}

	manifest, stats, err := buildManifestAndStats(p, params, inputDigest, paramsDigest, capRes, loud, color, validation, pcm)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, media.InternalWrap(media.StageReport, err, "encode manifest")
	}
	manifestBytes = append(manifestBytes, '\n')
	manifestHash := txn.DigestBytes(manifestBytes)
	stats.ManifestSHA256 = manifestHash
	statsLine := stats.KV() + "\n"
	if err := txn.WriteAtomic(p.Manifest, manifestBytes); err != nil {
		return nil, err
	}
	if err := txn.WriteAtomic(p.Stats, []byte(statsLine)); err != nil {
		return nil, err
	}
	reportArts, err := artifactsFor(p.Manifest, p.Stats)
	if err != nil {
		return nil, err
	}
	if _, err := wal.AppendCheckpoint(media.StageReport, inputDigest, paramsDigest, reportArts, "report generated"); err != nil {
		return nil, err
	}
	if err := txn.WriteAtomic(p.Committed, []byte("committed\n")); err != nil {
		return nil, err
	}
	if _, err := wal.AppendCheckpoint(media.StageCommit, inputDigest, paramsDigest, nil, "batch committed"); err != nil {
		return nil, err
	}
	return &Result{Stats: stats, Manifest: manifest}, nil
}

func loadOrBuildAudio(p *txn.Paths, params txn.Params, rec *txn.Recovery, capRes *CaptureResult) ([]byte, audio.LoudnessReport, error) {
	var loud audio.LoudnessReport
	if rec != nil && rec.StageDone(media.StageAudio) {
		pcm, err := os.ReadFile(p.AudioPCM)
		if err != nil {
			return nil, loud, media.InternalWrap(media.StageRecover, err, "reuse audio artifact")
		}
		if err := txn.ReadJSON(p.AudioJSON, &loud); err != nil {
			return nil, loud, err
		}
		return pcm, loud, nil
	}
	pcm, loud, err := BuildNormalizedAudio(capRes, params)
	if err != nil {
		return nil, loud, err
	}
	if err := txn.WriteAtomic(p.AudioPCM, pcm); err != nil {
		return nil, loud, err
	}
	if err := txn.WriteJSONAtomic(p.AudioJSON, loud); err != nil {
		return nil, loud, err
	}
	return pcm, loud, nil
}

func loadOrResolveColor(p *txn.Paths, params txn.Params, rec *txn.Recovery) (mp4.ColorDecision, error) {
	var color mp4.ColorDecision
	if rec != nil && rec.StageDone(media.StageColor) {
		if err := txn.ReadJSON(p.ColorJSON, &color); err != nil {
			return color, err
		}
		return color, nil
	}
	var source struct {
		ColorSources []mp4.ColorMetadata `json:"color_sources"`
	}
	if err := txn.ReadJSON(p.SourceJSON, &source); err != nil {
		return color, err
	}
	sources := source.ColorSources
	if len(sources) == 0 {
		sources = []mp4.ColorMetadata{{Source: "default", Standard: mp4.ColorBT709, Valid: true}}
	}
	if params.ColorConflict && len(sources) == 1 {
		sources = append(sources, mp4.ColorMetadata{Source: "conflict-flag", Standard: mp4.ColorBT2020, Valid: true})
	}
	color, err := mp4.ResolveColor(sources, mp4.DefaultColorConfig())
	if err != nil {
		return color, err
	}
	if err := txn.WriteJSONAtomic(p.ColorJSON, color); err != nil {
		return color, err
	}
	return color, nil
}

func loadOrMuxMP4(p *txn.Paths, rec *txn.Recovery, wal *txn.WAL, inputDigest, paramsDigest string, cfg mp4.MuxConfig) (*mp4.Validation, error) {
	var validation mp4.Validation
	if rec != nil && rec.StageDone(media.StageMP4) {
		if err := txn.ReadJSON(p.MP4JSON, &validation); err != nil {
			return nil, err
		}
		return &validation, nil
	}
	if err := txn.RejectSymlinkOutput(p.MP4); err != nil {
		return nil, err
	}
	tmp, err := txn.NewTempInSameDir(p.MP4)
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	_, merr := mp4.Mux(tmp, cfg)
	if merr != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, merr
	}
	if err := txn.CommitOpenFile(tmp, p.MP4); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	validationPtr, err := mp4.ValidateFile(p.MP4, cfg)
	if err != nil {
		return nil, err
	}
	validation = *validationPtr
	if err := txn.WriteJSONAtomic(p.MP4JSON, validation); err != nil {
		return nil, err
	}
	arts, err := artifactsFor(p.MP4, p.MP4JSON)
	if err != nil {
		return nil, err
	}
	if _, err := wal.AppendCheckpoint(media.StageMP4, inputDigest, paramsDigest, arts, "mp4 muxed and validated"); err != nil {
		return nil, err
	}
	return &validation, nil
}

func artifactsFor(paths ...string) ([]txn.Artifact, error) {
	arts := make([]txn.Artifact, 0, len(paths))
	for _, path := range paths {
		d, n, err := txn.SHA256File(path)
		if err != nil {
			return nil, err
		}
		arts = append(arts, txn.Artifact{Name: filepath.Base(path), Path: path, SHA256: d, Size: n})
	}
	return arts, nil
}
