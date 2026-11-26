package pipeline

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"avpipe/internal/abr"
	"avpipe/internal/media"
	"avpipe/internal/segment"
)

// SegmentReport is one representation-segment result row.
type SegmentReport struct {
	Group      int
	Tier       segment.Tier
	TargetKbps int
	ActualBps  int64
	DeltaPct   float64
	PSNR       float64
	DurationMS float64
	DriftMS    float64
	Gapped     bool
	Recovered  bool
	Path       string
	Hash       [32]byte
	BitrateOK  bool
}

func sourceDims() (int, int) { return segment.SourceWidth, segment.SourceHeight }

// encodeGroup encodes every representation for one second and immediately
// verifies bitrate tolerance, PSNR and alignment inside the staging dir.
func (r *Runner) encodeGroup(g uint32, groupDir string, tl *Timeline, tiers []segment.Tier, decision int) ([]SegmentReport, error) {
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		return nil, media.NewError(media.ClassIndex, "pipeline", "staging mkdir failed: "+err.Error(), 0, false)
	}
	start := g * uint32(media.VideoFPS)
	frames := tl.Frames[start : start+uint32(media.VideoFPS)]
	var audio bytes.Buffer
	for a := g * media.AudioPacketsPerSecond; a < (g+1)*media.AudioPacketsPerSecond; a++ {
		audio.Write(tl.Audio[a])
	}
	w, h := sourceDims()
	var rows []SegmentReport
	for _, tier := range tiers {
		name := fmt.Sprintf("v%dk-seg%d", tier.Kbps, g)
		res, err := segment.EncodeSegment(segment.EncodeRequest{
			OutDir: groupDir, Name: name, Frames: frames, Audio: audio.Bytes(),
			AudioPTS: media.AudioPTS(int(g) * media.AudioRate),
			Tier:     tier, SourceW: w, SourceH: h,
		})
		if err != nil {
			return nil, err
		}
		row, err := r.verifySegment(g, tier, res.SegmentPath, frames, tl)
		if err != nil {
			return nil, err
		}
		row.Gapped = groupHasGap(tl, g)
		if !row.BitrateOK {
			return nil, media.NewError(media.ClassEncode, "pipeline",
				fmt.Sprintf("bitrate deviation %.1f%% exceeds %d%% for %s",
					row.DeltaPct, int(abr.MaxBitrateDeltaPct), filepath.Base(row.Path)), 0, false)
		}
		rows = append(rows, row)
	}
	_ = decision // decision is recorded in the report but all tiers are rendered
	return rows, nil
}

// publishGroup moves verified segments to their variant directories and
// returns the committed per-variant hashes in target order.
func (r *Runner) publishGroup(g uint32, rows []SegmentReport, tiers []segment.Tier) ([][32]byte, error) {
	hashes := make([][32]byte, 0, len(tiers))
	for _, tier := range tiers {
		var row *SegmentReport
		for i := range rows {
			if rows[i].Tier.Kbps == tier.Kbps {
				row = &rows[i]
			}
		}
		if row == nil {
			return nil, media.NewError(media.ClassIndex, "pipeline", "missing variant in group", 0, false)
		}
		dir := filepath.Join(r.cfg.OutDir, segment.VariantDir(tier.Kbps))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, media.NewError(media.ClassIndex, "pipeline", "variant dir failed: "+err.Error(), 0, false)
		}
		final := filepath.Join(dir, fmt.Sprintf("seg%d.m4s", g))
		if err := os.Rename(row.Path, final); err != nil {
			return nil, media.NewError(media.ClassIndex, "pipeline", "publish rename failed: "+err.Error(), 0, false)
		}
		sum, err := hashFile(final)
		if err != nil {
			return nil, media.NewError(media.ClassIndex, "pipeline", "hash failed: "+err.Error(), 0, false)
		}
		row.Path = final
		row.Hash = sum
		hashes = append(hashes, sum)
	}
	return hashes, nil
}

// loadCommitted rebuilds report rows for groups produced before an interrupt.
// Each segment hash is re-checked against the journal before reuse, and the
// encoded metrics are recomputed from the on-disk segment so a resumed run
// reports the same numbers as an uninterrupted one.
func (r *Runner) loadCommitted(g uint32, tiers []segment.Tier, tl *Timeline) []SegmentReport {
	gr := r.state.Groups[g]
	start := g * uint32(media.VideoFPS)
	frames := tl.Frames[start : start+uint32(media.VideoFPS)]
	var rows []SegmentReport
	for i, tier := range tiers {
		path := filepath.Join(r.cfg.OutDir, segment.VariantDir(tier.Kbps), fmt.Sprintf("seg%d.m4s", g))
		sum, err := hashFile(path)
		ok := err == nil && i < len(gr.SegmentHashes) && sum == gr.SegmentHashes[i]
		if !ok {
			rows = append(rows, SegmentReport{
				Group: int(g), Tier: tier, TargetKbps: tier.Kbps,
				Gapped: gr.Gapped, Path: path, BitrateOK: false,
			})
			continue
		}
		row, verr := r.verifySegment(g, tier, path, frames, tl)
		if verr != nil {
			// A tampered committed segment is an index integrity failure.
			rows = append(rows, SegmentReport{
				Group: int(g), Tier: tier, TargetKbps: tier.Kbps,
				Gapped: gr.Gapped, Path: path, BitrateOK: false,
			})
			continue
		}
		row.Gapped = gr.Gapped
		row.Hash = sum
		rows = append(rows, row)
	}
	return rows
}

// finalizePlaylists renders aligned variant/master playlists from committed
// groups and hashes the master playlist for the determinism check.
func (r *Runner) finalizePlaylists() error {
	seconds := cfgSeconds(r.cfg)
	specs := make([]segment.SegmentSpec, seconds)
	for g := uint32(0); g < seconds; g++ {
		gapped := false
		if int(g) < len(r.state.Groups) {
			gapped = r.state.Groups[g].Gapped
		}
		specs[g] = segment.SegmentSpec{
			Index: int(g), Duration: float64(segment.SegmentDurationSeconds),
			Gap: gapped, URI: fmt.Sprintf("seg%d.m4s", g),
		}
	}
	master, variants, err := segment.BuildPlaylists(specs, r.tiers())
	if err != nil {
		return err
	}
	if err := segment.WritePlaylists(r.cfg.OutDir, master, variants); err != nil {
		return err
	}
	r.report.PlaylistHash = sha256.Sum256([]byte(master))
	return nil
}

// checkOutputDir validates the output path and creates the output tree.
// System-level symlinks above the deepest existing ancestor (for example
// /var -> /private/var on macOS) are resolved rather than rejected; any
// symlink inside the managed subtree is rejected as an escape attempt.
func checkOutputDir(dir string) error {
	if dir == "" {
		return media.NewError(media.ClassConfig, "pipeline", "empty output dir", 0, false)
	}
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return media.NewError(media.ClassConfig, "pipeline", "bad output dir", 0, false)
	}
	for _, part := range strings.Split(abs, string(filepath.Separator)) {
		if part == ".." {
			return media.NewError(media.ClassConfig, "pipeline", "path traversal rejected: "+dir, 0, false)
		}
	}
	// Deepest existing ancestor: resolve its real (symlink-free) location.
	existing := abs
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		parent := filepath.Dir(existing)
		if parent == existing {
			return media.NewError(media.ClassConfig, "pipeline", "cannot resolve output dir", 0, false)
		}
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return media.NewError(media.ClassConfig, "pipeline", "symlink resolution failed: "+err.Error(), 0, false)
	}
	target := real
	if len(tail) > 0 {
		target = filepath.Join(append([]string{real}, tail...)...)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return media.NewError(media.ClassConfig, "pipeline", "mkdir failed: "+err.Error(), 0, false)
	}
	// Guard the managed subtree: nothing below the real ancestor may be a link.
	walk := real
	for _, part := range tail {
		walk = filepath.Join(walk, part)
		if info, err := os.Lstat(walk); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return media.NewError(media.ClassConfig, "pipeline", "symlink escape rejected: "+walk, 0, false)
		}
	}
	return nil
}

var ffprobePath = func() string {
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p
	}
	return "ffprobe"
}()
