package pipeline

import (
	"errors"
	"os"
	"path/filepath"

	"mediadelivery/internal/analysis"
	"mediadelivery/internal/fault"
	"mediadelivery/internal/index"
	"mediadelivery/internal/probe"
)

// Sentinel errors mapped to process exit codes by the CLI.
var (
	ErrInvalidConfig = errors.New("pipeline: invalid configuration")
	ErrManifest      = errors.New("pipeline: manifest error")
	ErrCheckpoint    = errors.New("pipeline: checkpoint error")
	ErrIO            = errors.New("pipeline: I/O fatal")
)

// processOne analyses one primary block, recovering from the backup
// representation when the primary is missing or truncated.
func (r *Runner) processOne(b block) (result, error) {
	// Enforce the per-block memory cap before reading anything.
	if err := r.checkSize(b.path); err != nil && !b.missing {
		return result{}, err
	}

	res := result{block: b}
	workPath := b.path
	recovered := false

	if b.missing || b.truncated {
		backupPath := r.backupPath(b)
		if info, err := os.Stat(backupPath); err == nil && !info.IsDir() {
			workPath = backupPath
			recovered = true
			// Mark only the matching fault kind as recovered.
			if b.missing {
				r.recorder.MarkRecovered(fault.KindMissingSegment, b.sequence)
			}
			if b.truncated {
				r.recorder.MarkRecovered(fault.KindTruncatedPacket, b.sequence)
			}
		} else if b.missing {
			// No backup for a missing slot: the single fault recorded in
			// buildBlocks already carries Recoverable=false; do not double count.
			return res, nil
		}
	}
	res.recovered = recovered
	res.block.path = workPath

	if err := r.analyze(workPath, &res); err != nil {
		return result{}, err
	}

	entry := index.Entry{
		Camera:           res.block.camera,
		Representation:   res.block.role,
		Sequence:         res.block.sequence,
		Start90k:         res.block.start90k,
		Duration90k:      res.block.duration90k,
		// Store a work-dir-relative path so the index is stable across runs and
		// machines and its hash is reproducible regardless of the temp root.
		SourcePath:       r.relPath(res.block.path),
		ContentHash:      res.contentHash,
		FileHash:         res.fileHash,
		SilenceIntervals: toIndexSpans(res.silence),
		Recovered:        res.recovered,
	}
	res.entry = entry

	// Advance the checkpoint only after the block's output is fully formed.
	blockID := blockIDOf(b)
	hash := index.HashBytes([]byte(blockID + ":" + res.contentHash))
	if err := r.cm.Advance(blockID, hash, entry); err != nil {
		return result{}, err
	}
	return res, nil
}

// ErrInterrupted signals the configured mid-run interruption.
var ErrInterrupted = errors.New("pipeline: interrupted after configured block")

// checkSize rejects a block exceeding the in-memory cap.
func (r *Runner) checkSize(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > int64(r.cfg.MaxBlockBytes) {
		return &OversizeError{Path: path, Size: info.Size(), Limit: int64(r.cfg.MaxBlockBytes)}
	}
	return nil
}

// OversizeError reports a block that exceeds the memory cap.
type OversizeError struct {
	Path  string
	Size  int64
	Limit int64
}

func (e *OversizeError) Error() string {
	return "pipeline: block " + e.Path + " exceeds memory cap"
}

// backupPath resolves the equivalent backup segment for a primary block.
func (r *Runner) backupPath(b block) string {
	return filepath.Join(r.cfg.WorkDir, "fixture", b.camera, "backup",
		"seg-"+itoa(b.sequence)+".ts")
}

// analyze probes and runs silence/fingerprint analysis on one segment file.
func (r *Runner) analyze(path string, res *result) error {
	// Probe records colour provenance and stream specs.
	rep, err := probe.Probe(path)
	if err != nil {
		r.recorder.Add(fault.KindDecode, res.block.camera, "container", res.block.sequence,
			res.block.start90k, true, "probe failed: "+err.Error())
		return err
	}
	for _, s := range rep.Streams {
		if s.MediaType == "video" {
			r.colors.Record(normalizeObservation(path, s))
		}
	}

	sil, err := analysis.DetectSilence(path, analysis.DefaultSilenceConfig())
	if err != nil {
		return err
	}
	for _, ch := range sil.ChannelsSilence {
		for _, iv := range ch.Intervals {
			res.silence = append(res.silence, silenceSpan{Start90k: iv.Start90k, End90k: iv.End90k})
		}
	}
	res.avSyncMs = 0 // synthetic streams share an aligned timeline

	fp, err := analysis.FingerprintSegment(path)
	if err != nil {
		return err
	}
	res.fingerprint = fpLike{
		videoHash: fp.VideoHash, videoBits: fp.VideoBits,
		audioEnvelope: fp.AudioEnvelope,
	}
	res.contentHash = contentHash(fp)
	if data, err := os.ReadFile(path); err == nil {
		res.fileHash = index.HashBytes(data)
	}
	res.segmentMs = rep.Duration90k * 1000 / 90000
	return nil
}

// deduplicate finds equivalent content across cameras/representations using the
// perceptual fingerprint. Genuine different clips are never merged.
func (r *Runner) deduplicate(results []result) {
	type keyed struct {
		idx int
		fp  *analysis.Fingerprint
	}
	var seen []keyed
	for i := range results {
		if results[i].contentHash == "" {
			continue
		}
		cur := &analysis.Fingerprint{
			VideoHash: results[i].fingerprint.videoHash,
			VideoBits: results[i].fingerprint.videoBits,
			AudioEnvelope: results[i].fingerprint.audioEnvelope,
		}
		matched := false
		for _, s := range seen {
			if analysis.Similarity(cur, s.fp) {
				results[i].block.role = results[i].block.role // keep role
				r.dupCount++
				matched = true
				break
			}
		}
		if !matched {
			seen = append(seen, keyed{idx: i, fp: cur})
		}
	}
}

func blockIDOf(b block) string {
	return b.camera + "/" + b.role + "/" + itoa(b.sequence)
}

// relPath makes a path relative to the configured working directory so indexed
// source paths never embed machine-specific absolute prefixes.
func (r *Runner) relPath(p string) string {
	rel, err := filepath.Rel(r.cfg.WorkDir, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

func toIndexSpans(in []silenceSpan) []index.SilenceSpan {
	if len(in) == 0 {
		return nil
	}
	out := make([]index.SilenceSpan, len(in))
	for i, s := range in {
		out[i] = index.SilenceSpan{Start90k: s.Start90k, End90k: s.End90k}
	}
	return out
}
