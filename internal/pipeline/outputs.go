package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"avcore/internal/adapter"
	"avcore/internal/audit"
	"avcore/internal/caption"
	"avcore/internal/clock"
	"avcore/internal/manifest"
	"avcore/internal/segment"
	"avcore/internal/state"
)

// writeCaptions slices cues per cut and commits one VTT fragment per cut.
func (e *Engine) writeCaptions(cfg Config, plan *segment.Plan, caps caption.Result,
	journal *state.Journal, logger *audit.Logger) error {
	slices := segment.SliceCaptions(plan, caps.Cues)
	subDir := filepath.Join(cfg.JobDir, "subs")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return adapter.NewError(adapter.CodeInternal, "subs dir: "+err.Error(), adapter.Source{})
	}
	for _, sl := range slices {
		name := fmt.Sprintf("subs_%03d.vtt", sl.CutIndex)
		final := filepath.Join(subDir, name)
		var b strings.Builder
		b.WriteString("WEBVTT\n\n")
		for _, c := range sl.Cues {
			fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", c.Index,
				vttTime(c.StartTicks), vttTime(c.EndTicks), c.Text)
		}
		tmp := journal.TempPath(name)
		if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
			return adapter.NewError(adapter.CodeInternal, "caption write: "+err.Error(), adapter.Source{})
		}
		hash, err := state.FileSHA256(tmp)
		if err != nil {
			return err
		}
		if err := os.Rename(tmp, final); err != nil {
			return adapter.NewError(adapter.CodeInternal, "caption rename: "+err.Error(), adapter.Source{File: final})
		}
		journal.Captions = append(journal.Captions, state.CaptionRecord{
			CutIndex: sl.CutIndex, Path: filepath.ToSlash(filepath.Join("subs", name)), SHA256: hash,
		})
	}
	_ = logger
	return nil
}

// writeManifests renders and atomically writes every playlist from committed
// records only, then returns the SHA-256 of the combined manifest set.
func (e *Engine) writeManifests(cfg Config, plan *segment.Plan, journal *state.Journal) (string, error) {
	variants := make([]manifest.VariantSpec, len(cfg.Renditions))
	mediaURIs := make([]string, len(cfg.Renditions))
	var hasherInput strings.Builder
	for vi, rc := range cfg.Renditions {
		recs := recordsFor(journal, vi)
		if len(recs) != len(plan.Cuts) {
			return "", adapter.NewError(adapter.CodeMissingSegment,
				fmt.Sprintf("variant %d has %d/%d segments before manifest", vi, len(recs), len(plan.Cuts)),
				adapter.Source{})
		}
		spec := manifest.VariantSpec{
			Name: rc.Name, Bandwidth: uint32(rc.TargetBPS), Width: rc.Width, Height: rc.Height,
			FrameRate: float64(cfg.FrameRate.Num) / float64(cfg.FrameRate.Den), Codecs: "avc1.42c01e",
		}
		playlist, err := e.Builder.MediaPlaylist(recs, spec)
		if err != nil {
			return "", err
		}
		uri := fmt.Sprintf("v%d/index.m3u8", vi)
		if err := writeAtomic(filepath.Join(cfg.JobDir, uri), []byte(playlist)); err != nil {
			return "", err
		}
		variants[vi] = spec
		mediaURIs[vi] = uri
		hasherInput.WriteString(playlist)
	}
	captionDoc := &manifest.CaptionDoc{Language: "zh", Name: "中文", URI: "subs/index.m3u8"}
	if len(journal.Captions) > 0 {
		cpl, err := e.Builder.CaptionPlaylist(journal.Captions)
		if err != nil {
			return "", err
		}
		if err := writeAtomic(filepath.Join(cfg.JobDir, "subs", "index.m3u8"), []byte(cpl)); err != nil {
			return "", err
		}
		hasherInput.WriteString(cpl)
	}
	master, err := e.Builder.MasterPlaylist(variants, mediaURIs, captionDoc)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(filepath.Join(cfg.JobDir, "master.m3u8"), []byte(master)); err != nil {
		return "", err
	}
	hasherInput.WriteString(master)
	return state.HashBytes([]byte(hasherInput.String())), nil
}

// buildSummary assembles the audit summary, including cross-rendition
// alignment computed from every variant's committed cut starts.
func (e *Engine) buildSummary(cfg Config, plan *segment.Plan, journal *state.Journal,
	caps caption.Result, sha string, reused int) audit.Summary {
	sum := audit.Summary{
		Case: "normal", GeneratedAt: "", BadRecords: len(caps.BadRecords),
		ReusedSegments: reused, ManifestSHAEqual: true, ManifestSHA256: sha,
	}
	starts := make([][]int64, len(cfg.Renditions))
	for vi, rc := range cfg.Renditions {
		recs := recordsFor(journal, vi)
		vm := audit.ComputeVariant(rc.Name, rc.TargetBPS, recs)
		sum.Variants = append(sum.Variants, vm)
		for _, r := range recs {
			starts[vi] = append(starts[vi], r.StartTicks)
		}
	}
	sum.MaxAlignmentMS = audit.AlignmentMS(starts)
	return sum
}

func recordsFor(j *state.Journal, variant int) []state.SegmentRecord {
	out := make([]state.SegmentRecord, 0)
	for _, s := range j.Segments {
		if s.Variant == variant {
			out = append(out, s)
		}
	}
	return out
}

// vttTime renders 90 kHz ticks as an HH:MM:SS.mmm stamp.
func vttTime(ticks int64) string {
	totalMS := ticks * 1000 / clock.TicksPerSecond
	ms := totalMS % 1000
	s := (totalMS / 1000) % 60
	m := (totalMS / 60000) % 60
	h := totalMS / 3600000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// writeAtomic writes via a temp file and rename so a partial playlist can
// never be observed.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
