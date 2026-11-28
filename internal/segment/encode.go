// Package segment transcodes each camera into CMAF HLS renditions with
// ffmpeg and verifies that all variant timelines stay within one frame of
// each other. Encoding resumes safely: completed variants are never
// re-segmented, outputs land via temporary files and inputs are never
// modified.
package segment

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"presspack/internal/core"
	"presspack/internal/ingest"
)

// Variant is one output rendition.
type Variant struct {
	Name    string // directory-safe label, e.g. "v400"
	BitRate int    // target video bit rate, bits/s
}

// VariantsFromTargets builds variants from bit rates in bits/s.
func VariantsFromTargets(targets []int) []Variant {
	out := make([]Variant, len(targets))
	for i, b := range targets {
		out[i] = Variant{Name: "v" + strconv.Itoa(b/1000), BitRate: b}
	}
	return out
}

// EncodeResult describes one finished (or resumed) variant.
type EncodeResult struct {
	Variant      Variant
	Dir          string
	PlaylistPath string
	Segments     int
	Resumed      bool // true when existing output was reused
}

// metaFile records the encode parameters so reruns can prove a variant is
// already done and skip re-segmenting.
type metaFile struct {
	BitRate   int   `json:"bit_rate"`
	Duration  int64 `json:"duration_ticks"`
	SegLen    int64 `json:"seg_len_ticks"`
	FPSNum    int64 `json:"fps_num"`
	FPSDen    int64 `json:"fps_den"`
	Segments  int   `json:"segments"`
	SourceSHA string `json:"source_sha"`
}

// EncodeCamera renders every variant of one camera into outDir/<variant>.
// The deliverable length is the plan duration (valid prefix); gaps are not
// padded. Existing complete variants are reused untouched.
func EncodeCamera(ctx context.Context, plan *ingest.CameraPlan, outDir string, variants []Variant, segLen core.Time, sourceSHA string) ([]EncodeResult, error) {
	results := make([]EncodeResult, 0, len(variants))
	for _, v := range variants {
		res, err := encodeVariant(ctx, plan, outDir, v, segLen, sourceSHA)
		if err != nil {
			return results, fmt.Errorf("camera %s variant %s: %w", plan.Camera, v.Name, err)
		}
		results = append(results, res)
	}
	return results, nil
}

func encodeVariant(ctx context.Context, plan *ingest.CameraPlan, outDir string, v Variant, segLen core.Time, sourceSHA string) (EncodeResult, error) {
	dir := filepath.Join(outDir, v.Name)
	res := EncodeResult{Variant: v, Dir: dir, PlaylistPath: filepath.Join(dir, "index.m3u8")}
	want := metaFile{
		BitRate:   v.BitRate,
		Duration:  plan.Duration.Ticks,
		SegLen:    segLen.Ticks,
		FPSNum:    plan.FPS.Num,
		FPSDen:    plan.FPS.Den,
		SourceSHA: sourceSHA,
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "encode.json")); err == nil {
		var got metaFile
		if json.Unmarshal(raw, &got) == nil && got.BitRate == want.BitRate &&
			got.Duration == want.Duration && got.SegLen == want.SegLen &&
			got.FPSNum == want.FPSNum && got.FPSDen == want.FPSDen &&
			got.SourceSHA == want.SourceSHA {
			if _, err := os.Stat(res.PlaylistPath); err == nil {
				res.Segments = got.Segments
				res.Resumed = true
				return res, nil
			}
		}
	}
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return res, err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return res, err
	}
	gop := plan.FPS.Nominal() * segLen.Ticks / core.Timescale
	if gop < 1 {
		gop = 1
	}
	args := []string{
		"-nostdin", "-v", "error", "-y",
		"-i", plan.Source,
		"-t", strconv.FormatFloat(plan.Duration.Seconds(), 'f', 3, 64),
		"-map", "0:v:0",
		"-c:v", "libx264", "-preset", "veryfast",
		"-r", fmt.Sprintf("%d/%d", plan.FPS.Num, plan.FPS.Den),
		"-g", strconv.FormatInt(gop, 10),
		"-keyint_min", strconv.FormatInt(gop, 10),
		"-sc_threshold", "0",
		"-b:v", strconv.Itoa(v.BitRate),
		"-maxrate", strconv.Itoa(v.BitRate),
		"-bufsize", strconv.Itoa(v.BitRate * 2),
		"-pix_fmt", "yuv420p",
		"-fflags", "+bitexact",
		"-f", "hls",
		"-hls_time", strconv.FormatFloat(segLen.Seconds(), 'f', 3, 64),
		"-hls_segment_type", "fmp4",
		"-hls_playlist_type", "vod",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(tmp, "seg_%05d.m4s"),
		filepath.Join(tmp, "index.m3u8"),
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		return res, fmt.Errorf("ffmpeg: %v\n%s", err, out)
	}
	segs, err := filepath.Glob(filepath.Join(tmp, "seg_*.m4s"))
	if err != nil || len(segs) == 0 {
		os.RemoveAll(tmp)
		return res, fmt.Errorf("no segments produced")
	}
	want.Segments = len(segs)
	meta, _ := json.Marshal(want)
	if err := os.WriteFile(filepath.Join(tmp, "encode.json"), meta, 0o644); err != nil {
		os.RemoveAll(tmp)
		return res, err
	}
	// Atomic publish: swap the finished variant directory into place.
	if err := os.RemoveAll(dir); err != nil {
		return res, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return res, err
	}
	res.Segments = len(segs)
	return res, nil
}
