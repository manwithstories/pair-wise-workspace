package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"presspack/internal/adapter"
	"presspack/internal/audit"
	"presspack/internal/clock"
	"presspack/internal/core"
	"presspack/internal/ingest"
	"presspack/internal/manifest"
	"presspack/internal/segment"
	"presspack/internal/subtitle"
)

func lookPath(bin string) (string, error) { return exec.LookPath(bin) }

// pipeOptions parameterizes one pipeline run.
type pipeOptions struct {
	segLen  core.Time
	targets []int
}

// cameraInput groups the per-camera source files discovered on disk.
type cameraInput struct {
	name     string
	media    string
	subtitle string
	timecode string
}

// discoverCameras lists camera directories (sorted) and their assets.
func discoverCameras(srcDir string) ([]cameraInput, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, err
	}
	var cams []cameraInput
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cam := cameraInput{name: e.Name()}
		files, err := os.ReadDir(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			p := filepath.Join(srcDir, e.Name(), f.Name())
			switch strings.ToLower(filepath.Ext(f.Name())) {
			case ".mp4", ".ts", ".m4v", ".mov":
				cam.media = p
			case ".srt", ".vtt":
				cam.subtitle = p
			case ".txt":
				if strings.HasPrefix(f.Name(), "timecode") {
					cam.timecode = p
				}
			}
		}
		if cam.media != "" {
			cams = append(cams, cam)
		}
	}
	sort.Slice(cams, func(i, j int) bool { return cams[i].name < cams[j].name })
	return cams, nil
}

// expectedDuration reads the SMPTE timecode list: first line "fps=<rate>",
// then one label per interval. The expected deliverable duration is the
// last resolved time plus one interval.
func expectedDuration(path string) (core.Time, core.Faults, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return core.Time{}, nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "fps=") {
		return core.Time{}, nil, fmt.Errorf("timecode file %s: want fps= header and labels", path)
	}
	fps, err := clock.ParseFPS(strings.TrimSpace(strings.TrimPrefix(lines[0], "fps=")))
	if err != nil {
		return core.Time{}, nil, err
	}
	labels := make([]string, 0, len(lines)-1)
	for _, l := range lines[1:] {
		if s := strings.TrimSpace(l); s != "" {
			labels = append(labels, s)
		}
	}
	times, faults := clock.ResolveSequence(labels, fps)
	if faults.HasUnrecoverable() {
		return core.Time{}, faults, fmt.Errorf("timecode file %s has invalid labels", path)
	}
	if len(times) == 0 {
		return core.Time{}, faults, fmt.Errorf("timecode file %s: no labels", path)
	}
	last := times[len(times)-1]
	step := fps.FrameDuration()
	if len(times) > 1 {
		step = last.Sub(times[len(times)-2])
	}
	return last.Add(step), faults, nil
}

// runPipeline processes every camera under srcDir into outDir and returns
// the run report. Unrecoverable cameras fail individually; the rest of the
// batch continues.
func runPipeline(ctx context.Context, srcDir, outDir string, opts pipeOptions) (*Report, error) {
	cams, err := discoverCameras(srcDir)
	if err != nil {
		return nil, err
	}
	if len(cams) == 0 {
		return nil, fmt.Errorf("no camera inputs under %s", srcDir)
	}
	rep := &Report{SegmentMs: opts.segLen.Millis(), Targets: opts.targets}
	ad := adapter.FFprobeV1{}
	variants := segment.VariantsFromTargets(opts.targets)
	for _, cam := range cams {
		rep.Cameras = append(rep.Cameras, processCamera(ctx, ad, cam, outDir, variants, opts.segLen))
	}
	hash, err := audit.HashTree(outDir)
	if err != nil {
		return rep, err
	}
	rep.DeliveryHash = hash
	return rep, nil
}

func processCamera(ctx context.Context, ad adapter.Adapter, cam cameraInput, outDir string, variants []segment.Variant, segLen core.Time) CameraReport {
	cr := CameraReport{Name: cam.name}
	var expect core.Time
	if cam.timecode != "" {
		exp, faults, err := expectedDuration(cam.timecode)
		cr.Faults = append(cr.Faults, faults...)
		if err != nil {
			cr.Error = err.Error()
			return cr
		}
		expect = exp
	}
	plan, err := ingest.BuildPlan(ctx, ad, cam.name, cam.media, expect)
	cr.Faults = append(cr.Faults, plan.Faults...)
	cr.Records = plan.Records
	if err != nil {
		cr.Error = err.Error()
		return cr
	}
	cr.DurationTicks = plan.Duration.Ticks

	camOut := filepath.Join(outDir, cam.name)
	srcSHA, err := hashFile(cam.media)
	if err != nil {
		cr.Error = err.Error()
		return cr
	}
	results, err := segment.EncodeCamera(ctx, plan, camOut, variants, segLen, srcSHA)
	if err != nil {
		cr.Error = err.Error()
		return cr
	}
	if len(results) > 0 {
		cr.SegmentsPerVariant = results[0].Segments
	}

	// Subtitles: parse once, slice onto the segment grid.
	var subInfo *manifest.SubtitleInfo
	if cam.subtitle != "" {
		cues, faults := parseSubtitleFile(cam.subtitle)
		cr.Faults = append(cr.Faults, faults...)
		uri, fitMs, err := writeSubtitleSegments(camOut, cues, plan.Duration, segLen)
		if err != nil {
			cr.Error = err.Error()
			return cr
		}
		cr.SubtitleFitMs = fitMs
		subInfo = &manifest.SubtitleInfo{GroupID: "subs", Name: "English", Language: "en", URI: uri}
	}

	// Alignment across variants, read back from the written playlists.
	starts := map[string][]core.Time{}
	for _, r := range results {
		durs, err := manifest.ReadSegmentDurations(r.PlaylistPath)
		if err != nil {
			cr.Error = err.Error()
			return cr
		}
		starts[r.Variant.Name] = segment.SegmentStarts(durs)
	}
	align, alignFaults := segment.CheckAlignment(starts, plan.FPS.FrameDuration())
	cr.Faults = append(cr.Faults, alignFaults...)
	cr.MaxAlignMs = align.MaxError.Millis()

	vinfos := make([]manifest.VariantInfo, len(variants))
	for i, v := range variants {
		vinfos[i] = manifest.VariantInfo{
			Name: v.Name, PlaylistFile: "index.m3u8",
			Bandwidth: uint32(v.BitRate), Width: 640, Height: 360,
			Codecs: "avc1.4d401f", FrameRate: fpsFloat(plan.FPS),
		}
	}
	if err := manifest.WriteMaster(filepath.Join(camOut, "master.m3u8"), vinfos, subInfo, faultNotices(cr.Faults)); err != nil {
		cr.Error = err.Error()
		return cr
	}
	cr.Delivered = true
	return cr
}

// writeSubtitleSegments crops cues onto the segment grid and writes one VTT
// per segment plus the subtitle media playlist. Returns the playlist URI
// (relative to the camera dir) and the subtitle/media fit in ms.
func writeSubtitleSegments(camOut string, cues []subtitle.Cue, duration, segLen core.Time) (string, int64, error) {
	subDir := filepath.Join(camOut, "subs")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return "", 0, err
	}
	n := int((duration.Ticks + segLen.Ticks - 1) / segLen.Ticks)
	if n < 1 {
		n = 1
	}
	names := make([]string, n)
	durs := make([]core.Time, n)
	for i := 0; i < n; i++ {
		start := core.Time{Ticks: int64(i) * segLen.Ticks}
		end := start.Add(segLen)
		if end.Cmp(duration) > 0 {
			end = duration
		}
		if end.Cmp(start) <= 0 {
			end = start.Add(segLen) // keep grid uniform past the tail
		}
		names[i] = fmt.Sprintf("seg_%05d.vtt", i)
		durs[i] = end.Sub(start)
		body := subtitle.RenderVTT(subtitle.Crop(cues, start, end))
		tmp := filepath.Join(subDir, "."+names[i]+".tmp")
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			return "", 0, err
		}
		if err := os.Rename(tmp, filepath.Join(subDir, names[i])); err != nil {
			return "", 0, err
		}
	}
	if err := manifest.WriteSubtitlePlaylist(filepath.Join(subDir, "index.m3u8"), names, durs); err != nil {
		return "", 0, err
	}
	var lastEnd core.Time
	for _, c := range cues {
		if c.End.Cmp(lastEnd) > 0 {
			lastEnd = c.End
		}
	}
	fit, _ := segment.CheckSubtitleFit(lastEnd, duration, core.FromMillis(audit.MaxAlignMs))
	return "subs/index.m3u8", fit.Millis(), nil
}

func parseSubtitleFile(path string) ([]subtitle.Cue, core.Faults) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, core.Faults{core.NewFault(core.FaultBadCue, -1, true, err.Error())}
	}
	return subtitle.Parse(raw)
}

func faultNotices(faults core.Faults) []string {
	var out []string
	for _, f := range faults {
		out = append(out, fmt.Sprintf("kind=%s offset=%d recoverable=%t detail=%q",
			f.Kind, f.Offset, f.Recoverable, f.Detail))
	}
	return out
}

func fpsFloat(f clock.FPS) float64 { return float64(f.Num) / float64(f.Den) }

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
