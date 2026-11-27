// Package encode drives the local FFmpeg subprocess: it pins the tool version
// into the processing credentials and cache key, transcodes a camera into each
// ladder rendition, and muxes fMP4 HLS segments whose cuts land on IDR frames.
// All invocations use deterministic flags so repeated runs are byte-stable.
package encode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/fashionweek/mediad/internal/media"
)

// Runner locates and versions the FFmpeg toolchain.
type Runner struct {
	ffmpeg  string
	ffprobe string
	version string
}

// New resolves ffmpeg/ffprobe from PATH and records their version.
func New(ctx context.Context) (*Runner, error) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, media.New(media.PhaseEncode, media.KindTool, "ffmpeg", -1,
			"ffmpeg not found on PATH; run `brew install ffmpeg`").Wrap(err)
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, media.New(media.PhaseEncode, media.KindTool, "ffprobe", -1,
			"ffprobe not found on PATH").Wrap(err)
	}
	out, err := exec.CommandContext(ctx, ff, "-version").Output()
	if err != nil {
		return nil, media.New(media.PhaseEncode, media.KindTool, ff, -1,
			"cannot read ffmpeg version").Wrap(err)
	}
	ver := "unknown"
	if line := strings.SplitN(string(out), "\n", 2); len(line) > 0 {
		ver = strings.TrimSpace(line[0])
	}
	return &Runner{ffmpeg: ff, ffprobe: fp, version: ver}, nil
}

// Version returns the full first-line version string used in cache keys.
func (r *Runner) Version() string { return r.version }

// FFmpegPath / FFprobePath expose the resolved binaries.
func (r *Runner) FFmpegPath() string  { return r.ffmpeg }
func (r *Runner) FFprobePath() string { return r.ffprobe }

// RenditionRequest is one transcode+mux target within a camera.
type RenditionRequest struct {
	Input        string
	OutDir       string // directory receiving init.mp4 and seg-NNN.m4s
	Width        int
	Height       int
	VideoBitrate string // e.g. "1200k"
	AudioBitrate string // e.g. "64k"
	GOP          int    // keyframe interval in frames
	SegmentSecs  int
	Variant      string
}

// RenditionResult lists the produced init segment and media segments.
type RenditionResult struct {
	Init     string
	Segments []string
}

// Transcode runs a deterministic H.264/AAC transcode and fMP4 segmentation.
// The HLS muxer emits an init section plus cut-on-IDR .m4s segments; the
// generated .m3u8 is discarded because the pipeline authors manifests itself.
func (r *Runner) Transcode(ctx context.Context, req RenditionRequest) (*RenditionResult, error) {
	pattern := fmt.Sprintf("%s/seg-%%03d.m4s", req.OutDir)
	initPath := req.OutDir + "/init.mp4"
	// The muxer needs a playlist path even though we do not keep the file.
	dummyPlaylist := req.OutDir + "/.ffmpeg.m3u8"
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", req.Input,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "libx264",
		"-preset", "medium",
		"-b:v", req.VideoBitrate,
		"-pix_fmt", "yuv420p",
		"-g", fmt.Sprint(req.GOP),
		"-keyint_min", fmt.Sprint(req.GOP),
		"-sc_threshold", "0",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", req.SegmentSecs),
		"-c:a", "aac", "-b:a", req.AudioBitrate, "-ar", "48000", "-ac", "2",
		"-fflags", "+bitexact", "-flags", "+bitexact",
		"-map_metadata", "-1",
		"-f", "hls",
		"-hls_time", fmt.Sprint(req.SegmentSecs),
		"-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", pattern,
		dummyPlaylist,
	}
	if err := r.run(ctx, args, req.Input); err != nil {
		return nil, err
	}
	segs, err := listSegments(req.OutDir)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, media.New(media.PhaseEncode, media.KindTool, req.Input, -1,
			"transcode produced no segments")
	}
	return &RenditionResult{Init: initPath, Segments: segs}, nil
}

// ProbeDuration returns the container duration in seconds via ffprobe.
func (r *Runner) ProbeDuration(ctx context.Context, path string) (float64, error) {
	args := []string{"-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path}
	cmd := exec.CommandContext(ctx, r.ffprobe, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, media.New(media.PhaseEncode, media.KindTool, path, -1,
			"ffprobe duration failed").Wrap(err)
	}
	var d float64
	if _, err := fmt.Sscanf(strings.TrimSpace(out.String()), "%f", &d); err != nil {
		return 0, media.New(media.PhaseEncode, media.KindTool, path, -1,
			"cannot parse ffprobe duration").Wrap(err)
	}
	return d, nil
}

func (r *Runner) run(ctx context.Context, args []string, input string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.ffmpeg, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return media.New(media.PhaseEncode, media.KindTool, input, -1,
			"ffmpeg failed: "+strings.TrimSpace(stderr.String())).Wrap(err)
	}
	return nil
}

func listSegments(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, media.New(media.PhaseEncode, media.KindTool, dir, -1,
			"cannot read transcode output directory").Wrap(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "seg-") && strings.HasSuffix(name, ".m4s") {
			out = append(out, dir+"/"+name)
		}
	}
	sort.Strings(out)
	return out, nil
}
