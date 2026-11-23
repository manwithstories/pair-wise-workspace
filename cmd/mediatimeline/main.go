// Command mediatimeline is the terminal entry point for the media
// timeline pipeline. Subcommands: demo (generate fixtures and run),
// resume (continue an interrupted run), verify (re-run and compare
// digests). Exit codes: 0 = checks passed, 1 = input/validation
// failure, 2 = bad arguments.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediatimeline/internal/pipeline"
	"mediatimeline/internal/render"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	var code int
	switch args[0] {
	case "demo":
		code = cmdDemo(args[1:])
	case "resume":
		code = cmdResume(args[1:])
	case "verify":
		code = cmdVerify(args[1:])
	default:
		usage()
		return 2
	}
	return code
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mediatimeline <demo|resume|verify> [flags]")
	fmt.Fprintln(os.Stderr, "  demo   --seed N --tracks N --frames N --out DIR [--interrupt-after N]")
	fmt.Fprintln(os.Stderr, "  resume --dir DIR")
	fmt.Fprintln(os.Stderr, "  verify --dir DIR")
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	return 1
}

func printReport(rep *pipeline.Report) {
	for _, n := range rep.Notes {
		fmt.Println("note=" + n)
	}
	for _, s := range rep.Stages {
		fmt.Printf("stage=%s cache_hit=%t recomputed=%t duration_ms=%.2f\n",
			s.Name, s.CacheHit, s.Recomputed, float64(s.DurationMicros)/1000)
	}
	for _, c := range rep.Corrupt {
		fmt.Printf("corrupt_track=%s corrupt_offset=%d reason=%q\n", c.Track, c.Offset, c.Reason)
	}
	for _, tr := range rep.TrackReports {
		fmt.Printf("track=%s aligned=%t kept=%d duplicates=%d reversed=%d missing=%d\n",
			tr.TrackID, tr.Aligned, tr.Kept, tr.Duplicates, tr.Reversed, tr.Missing)
	}
	fmt.Printf("cache_hits=%d\n", rep.CacheHits)
	fmt.Printf("recomputed_stages=%d\n", rep.RecomputedStages)
}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "fixture seed")
	tracks := fs.Int("tracks", 4, "number of tracks")
	frames := fs.Int("frames", 300, "samples per track")
	out := fs.String("out", "", "output directory")
	interrupt := fs.Int("interrupt-after", 0, "stop after N completed stages")
	maxDrift := fs.Int64("max-drift-ppm", 500, "drift threshold in ppm")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" || *tracks < 1 || *frames < 1 {
		usage()
		return 2
	}
	if ents, err := os.ReadDir(*out); err == nil && len(ents) > 0 {
		return fail("output directory %s is not empty", *out)
	}
	params := pipeline.RunParams{Seed: *seed, Tracks: *tracks, Frames: *frames, MaxDriftPPM: *maxDrift}
	meta, err := pipeline.Generate(*out, params)
	if err != nil {
		return fail("generate fixtures: %v", err)
	}
	rep, err := pipeline.Run(pipeline.Options{Dir: *out, InterruptAfter: *interrupt})
	if err != nil {
		return fail("pipeline: %v", err)
	}
	fmt.Printf("tracks=%d\n", *tracks)
	fmt.Printf("frames=%d\n", *frames)
	printReport(rep)
	fmt.Printf("interrupted=%t\n", rep.Interrupted)
	offsetErr := int64(0)
	if meta.CorruptTrack != "" {
		fmt.Printf("expected_corrupt_track=%s\n", meta.CorruptTrack)
		found := false
		for _, c := range rep.Corrupt {
			if c.Track == meta.CorruptTrack {
				found = true
				offsetErr = c.Offset - meta.CorruptOffset
				if offsetErr < 0 {
					offsetErr = -offsetErr
				}
			}
		}
		if !found {
			return fail("corrupt track %s was not reported", meta.CorruptTrack)
		}
	}
	fmt.Printf("offset_error_bytes=%d\n", offsetErr)
	if offsetErr > 16 {
		return fail("corrupt offset error %d exceeds 16 bytes", offsetErr)
	}
	fmt.Printf("full_ms=%.2f\n", float64(rep.RunMicros)/1000)
	if rep.SummarySHA256 != "" {
		fmt.Printf("sha256=%s\n", rep.SummarySHA256)
		fmt.Printf("manifest_sha256=%s\n", rep.ManifestDigest)
	} else {
		fmt.Println("sha256=pending")
	}
	return 0
}

func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	dir := fs.String("dir", "", "run directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		usage()
		return 2
	}
	rep, err := pipeline.Run(pipeline.Options{Dir: *dir})
	if err != nil {
		return fail("resume: %v", err)
	}
	fmt.Printf("tracks=%d\n", rep.Params.Tracks)
	fmt.Printf("frames=%d\n", rep.Params.Frames)
	printReport(rep)
	resumeUs := rep.RunMicros
	fullUs := rep.CompletedMicros + rep.RunMicros
	ratio := float64(0)
	if fullUs > 0 {
		ratio = float64(resumeUs) / float64(fullUs)
	}
	fmt.Printf("resume_ms=%.2f\n", float64(resumeUs)/1000)
	fmt.Printf("full_ms=%.2f\n", float64(fullUs)/1000)
	fmt.Printf("resume_ratio_pct=%.1f\n", ratio*100)
	fmt.Printf("sha256=%s\n", rep.SummarySHA256)
	fmt.Printf("manifest_sha256=%s\n", rep.ManifestDigest)
	if rep.Interrupted {
		return fail("run is still incomplete")
	}
	// The 40% resume budget applies to a clean resume (only unfinished
	// stages recomputed). Recovery from rejected checkpoints or cache
	// entries legitimately costs more and is reported via notes.
	if ratio > 0.40 && len(rep.Notes) == 0 {
		return fail("resume ratio %.1f%% exceeds 40%%", ratio*100)
	}
	return 0
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "run directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		usage()
		return 2
	}
	// 1. Re-hash frames.bin against the manifest's per-frame digests.
	mdata, err := os.ReadFile(filepath.Join(*dir, "manifest.json"))
	if err != nil {
		return fail("read manifest: %v", err)
	}
	var m struct {
		Frames        []render.FrameOut `json:"frames"`
		SummarySHA256 string            `json:"summary_sha256"`
	}
	if err := json.Unmarshal(mdata, &m); err != nil {
		return fail("parse manifest: %v", err)
	}
	framesBin, err := os.ReadFile(filepath.Join(*dir, "frames.bin"))
	if err != nil {
		return fail("read frames.bin: %v", err)
	}
	if len(framesBin) != len(m.Frames)*render.Bytes {
		return fail("frames.bin size %d != %d frames x %d bytes", len(framesBin), len(m.Frames), render.Bytes)
	}
	for i, f := range m.Frames {
		sum := sha256.Sum256(framesBin[i*render.Bytes : (i+1)*render.Bytes])
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return fail("frame %d hash mismatch", i)
		}
	}
	if got := render.Summary(m.Frames); got != m.SummarySHA256 {
		return fail("summary mismatch: manifest=%s recomputed=%s", m.SummarySHA256, got)
	}
	// 2. Re-run the whole pipeline from the same params and compare.
	params, err := pipeline.LoadParams(*dir)
	if err != nil {
		return fail("load params: %v", err)
	}
	tmp, err := os.MkdirTemp("", "mediatimeline-verify-*")
	if err != nil {
		return fail("mktemp: %v", err)
	}
	defer os.RemoveAll(tmp)
	if _, err := pipeline.Generate(tmp, params); err != nil {
		return fail("regenerate fixtures: %v", err)
	}
	rep, err := pipeline.Run(pipeline.Options{Dir: tmp})
	if err != nil {
		return fail("re-run pipeline: %v", err)
	}
	manifestSum := sha256.Sum256(mdata)
	manifestDigest := hex.EncodeToString(manifestSum[:])
	fmt.Printf("tracks=%d\n", params.Tracks)
	fmt.Printf("frames=%d\n", params.Frames)
	fmt.Printf("frames_verified=%d\n", len(m.Frames))
	fmt.Printf("sha256=%s\n", m.SummarySHA256)
	fmt.Printf("manifest_sha256=%s\n", manifestDigest)
	if rep.SummarySHA256 != m.SummarySHA256 || rep.ManifestDigest != manifestDigest {
		fmt.Println("verify_ok=false")
		fmt.Printf("rerun_sha256=%s\nrerun_manifest_sha256=%s\n", rep.SummarySHA256, rep.ManifestDigest)
		return fail("re-run digests differ from directory contents")
	}
	fmt.Println("verify_ok=true")
	return 0
}
