// Package stream executes the segmentation plan: it transwraps each
// rendition into HLS segments via ffmpeg, normalizing audio and color per
// policy, writing through a temp directory with atomic commits. With
// Resume, only segments whose recorded digest still verifies are reused.
package stream

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"aircheck/internal/clock"
	"aircheck/internal/fault"
	"aircheck/internal/normalize"
	"aircheck/internal/plan"
	"aircheck/internal/playlist"
	"aircheck/internal/scan"
)

// Options configures one run.
type Options struct {
	OutDir string
	Resume bool
	Audio  normalize.AudioPolicy
	Color  normalize.ColorPolicy
}

// segState records one committed segment for resume verification.
type segState struct {
	URI           string `json:"uri"`
	SHA256        string `json:"sha256"`
	DurationTicks int64  `json:"duration_ticks"`
}

type runState struct {
	Key      string     `json:"key"`
	Segments []segState `json:"segments"`
}

// Notes collects normalization decisions for the run log.
type Notes []string

// RunAll segments every rendition and writes playlists. It returns
// normalization notes; faults abort with track/tick context.
func RunAll(res *scan.Result, cp *plan.CutPlan, inputRoot, cacheKey string, o Options) (Notes, error) {
	var notes Notes
	vars := make([]playlist.Variant, 0, len(res.Renditions))
	for _, r := range res.Renditions {
		v, n, err := runRendition(r, cp, inputRoot, cacheKey, o)
		if err != nil {
			return notes, err
		}
		notes = append(notes, n...)
		vars = append(vars, v)
	}
	if err := playlist.WriteMaster(o.OutDir, vars); err != nil {
		return notes, err
	}
	return notes, nil
}

func runRendition(r scan.Rendition, cp *plan.CutPlan, inputRoot, cacheKey string, o Options) (playlist.Variant, Notes, error) {
	var notes Notes
	v := playlist.Variant{Name: r.Name, URI: r.Name + "/playlist.m3u8", Bandwidth: uint32(r.TargetBPS)}
	finalDir := filepath.Join(o.OutDir, r.Name)
	tmpDir := filepath.Join(o.OutDir, ".tmp", r.Name)
	if !o.Resume {
		os.RemoveAll(finalDir)
	}
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return v, notes, err
	}
	defer os.RemoveAll(tmpDir)

	args, n, err := ffmpegArgs(r, cp, inputRoot, tmpDir, o)
	if err != nil {
		return v, notes, err
	}
	notes = append(notes, n...)
	if out, err := runFFmpeg(args); err != nil {
		return v, notes, fault.New(fault.CodeRuntime, r.Name, -1, false,
			"ffmpeg segmentation failed: %v: %s", err, tail(out))
	}

	segs, err := measureSegments(tmpDir)
	if err != nil {
		return v, notes, err
	}
	if len(segs) != cp.NumSegments() {
		return v, notes, fault.New(fault.CodeSegmentCorrupt, r.Name, -1, true,
			"produced %d segments, plan has %d", len(segs), cp.NumSegments())
	}
	if err := commitSegments(finalDir, tmpDir, segs, cacheKey, o.Resume); err != nil {
		return v, notes, err
	}
	plSegs := make([]playlist.Segment, len(segs))
	for i, s := range segs {
		plSegs[i] = playlist.Segment{URI: s.URI, DurationTicks: s.DurationTicks}
	}
	if err := playlist.WriteMediaPlaylist(finalDir, plSegs); err != nil {
		return v, notes, err
	}
	for _, t := range r.Tracks {
		if t.Kind == "video" {
			v.Resolution = fmt.Sprintf("%dx%d", t.Width, t.Height)
			v.Codecs = "avc1.640028,mp4a.40.2"
		}
	}
	return v, notes, nil
}

// ffmpegArgs builds the segmentation command from normalize decisions.
func ffmpegArgs(r scan.Rendition, cp *plan.CutPlan, inputRoot, tmpDir string, o Options) ([]string, Notes, error) {
	var notes Notes
	in := filepath.Join(inputRoot, r.File)
	args := []string{"-hide_banner", "-v", "error", "-i", in,
		"-map", "0:v:0", "-map", "0:a:0?"}
	reencode := false
	for _, t := range r.Tracks {
		switch t.Kind {
		case "audio":
			d, err := normalize.PlanAudio(t, o.Audio)
			if err != nil {
				return nil, notes, err
			}
			notes = append(notes, d.Notes...)
			args = append(args, d.Args()...)
		case "video":
			d, err := normalize.CheckColor(t, o.Color)
			if err != nil {
				return nil, notes, err
			}
			notes = append(notes, d.Notes...)
			if d.Retag {
				reencode = true
			}
		}
	}
	segSec := strconv.FormatFloat(clock.Seconds(cp.SegmentTicks), 'f', 3, 64)
	if reencode {
		vbps := r.TargetBPS - 128000
		if vbps < 100000 {
			vbps = 100000
		}
		gop := int(clock.ToRational(cp.SegmentTicks, 25))
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast",
			"-threads", "1", // deterministic output for resume/hash reuse
			"-b:v", fmt.Sprint(vbps), "-minrate", fmt.Sprint(vbps),
			"-maxrate", fmt.Sprint(vbps),
			"-bufsize", fmt.Sprint(vbps/2), "-g", fmt.Sprint(gop),
			"-force_key_frames", "expr:gte(t,n_forced*"+segSec+")",
			"-pix_fmt", "yuv420p")
		args = append(args, normalize.ColorArgs(o.Color.Target)...)
	} else {
		args = append(args, "-c:v", "copy")
	}
	args = append(args, "-c:a", "aac", "-b:a", "128k",
		"-f", "segment", "-segment_time", segSec, "-reset_timestamps", "1",
		"-segment_format", "mpegts",
		filepath.Join(tmpDir, "seg%05d.ts"))
	return args, notes, nil
}

func runFFmpeg(args []string) (string, error) {
	cmd := exec.Command("ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}
