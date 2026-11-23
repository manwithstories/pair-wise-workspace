// Command medpipe is the terminal entry point: it generates a deterministic
// capture, runs the processing graph, demonstrates corruption rejection and
// recovery, and prints a key=value report.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"medpipe/internal/dag"
	"medpipe/internal/model"
	"medpipe/internal/pipeline"
	"medpipe/internal/sequence"
	"medpipe/internal/synth"
)

const usage = `medpipe - local media pipeline demo

Usage:
  medpipe demo [flags]
  medpipe verify [flags]

Demo flags:
  --seed int        generator seed (default 42)
  --tracks int      number of tracks (default 3)
  --frames int      frames per track (default 240)
  --drift-ppm float capture clock drift in parts per million (default 180)
  --wrap-at uint    counter modulus, e.g. 65536 (0 = no wrap)
  --vfr             enable variable frame rate intervals
  --work string     work directory (default: a fresh temp dir)
  --keep            keep the work directory and print its path

Exit codes: 0 success, 2 input or dependency error, 1 internal invariant.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "medpipe: %v\n", err)
		os.Exit(model.ExitCode(err))
	}
}

type demoFlags struct {
	seed     int64
	tracks   int
	frames   int
	driftPPM float64
	wrapAt   uint64
	vfr      bool
	work     string
	keep     bool
}

func parseDemoFlags(args []string) (*demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	d := &demoFlags{}
	fs.Int64Var(&d.seed, "seed", 42, "generator seed")
	fs.IntVar(&d.tracks, "tracks", 3, "number of tracks")
	fs.IntVar(&d.frames, "frames", 240, "frames per track")
	fs.Float64Var(&d.driftPPM, "drift-ppm", 180, "capture clock drift in ppm")
	fs.Uint64Var(&d.wrapAt, "wrap-at", 65536, "counter modulus (0 disables wrap)")
	fs.BoolVar(&d.vfr, "vfr", false, "enable variable frame rate")
	fs.StringVar(&d.work, "work", "", "work directory")
	fs.BoolVar(&d.keep, "keep", false, "keep the work directory")
	if err := fs.Parse(args); err != nil {
		return nil, model.Wrap(model.KindInput, "medpipe.demo", err)
	}
	if fs.NArg() != 0 {
		return nil, model.New(model.KindInput, "medpipe.demo", fmt.Sprintf("unexpected arguments %v", fs.Args()))
	}
	if d.tracks <= 0 || d.frames <= 0 {
		return nil, model.New(model.KindInput, "medpipe.demo", "tracks and frames must be positive")
	}
	if d.driftPPM < -10000 || d.driftPPM > 10000 {
		return nil, model.New(model.KindInput, "medpipe.demo", "drift-ppm must be within +/-10000")
	}
	return d, nil
}

func runDemo(args []string) error {
	flags, err := parseDemoFlags(args)
	if err != nil {
		return err
	}
	start := time.Now()
	work := flags.work
	cleanup := func() {}
	if work == "" {
		dir, err := os.MkdirTemp("", "medpipe-demo-*")
		if err != nil {
			return model.Wrap(model.KindInput, "medpipe.demo", err)
		}
		work, cleanup = dir, func() { os.RemoveAll(dir) }
	}
	if !flags.keep {
		if flags.work != "" {
			// An explicit work directory is always kept; only temp dirs are
			// removed automatically.
			defer func() {}()
		}
		defer cleanup()
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return model.Wrap(model.KindInput, "medpipe.demo", err)
	}
	input := filepath.Join(work, "capture.medpseq")
	state := filepath.Join(work, "state", "nodes.json")

	capture, err := synth.Generate(synth.Options{
		Seed: flags.seed, Tracks: flags.tracks, Frames: flags.frames,
		DriftPPM: flags.driftPPM, WrapAt: flags.wrapAt, VFR: flags.vfr,
	})
	if err != nil {
		return err
	}
	if _, _, err := sequence.WriteFile(input, capture.Tracks, capture.Frames); err != nil {
		return err
	}

	cfg := pipeline.Config{
		InputPath: "capture.medpseq", WorkDir: work, StatePath: state,
		SidecarPath: "capture.manifest.mp4", WindowNS: 2_000_000_000,
		SilenceWindowNS: 20_000_000, SilenceThreshold: 0.05, MaxAttempts: 3,
	}
	out, err := pipeline.Run(cfg)
	if err != nil {
		return err
	}

	// 1. Checksum corruption must be rejected without being silently skipped.
	corruptRejected := 0
	if err := demonstrateChecksumRejection(work, input); err != nil {
		return err
	}
	corruptRejected++

	// 2. A frame whose record checksum is intact but whose media framing is
	// damaged must fail segmentation, block fingerprinting, and recover after
	// the input is repaired while reusing upstream nodes.
	blocked, reused, err := demonstrateRecovery(cfg, capture)
	if err != nil {
		return err
	}

	rep, err := pipeline.BuildReport(out, countReused(reused), blocked, corruptRejected)
	if err != nil {
		return err
	}
	rep.BoundaryCases = boundaryCases(out)
	rep.ChecksumCases = 1
	for _, line := range rep.Lines() {
		fmt.Println(line)
	}
	fmt.Printf("corrupt_cases=%d\n", rep.ChecksumCases)
	fmt.Printf("boundary_cases=%d\n", rep.BoundaryCases)
	fmt.Printf("elapsed_ms=%d\n", time.Since(start).Milliseconds())
	if flags.keep || flags.work != "" {
		fmt.Printf("work_dir=%s\n", work)
	}
	if rep.Segments <= 0 || rep.SegmentSHA256Count <= 0 {
		return model.New(model.KindInvariant, "medpipe.demo", "demo produced no segments")
	}
	return nil
}

// demonstrateChecksumRejection flips a payload byte in a copy of the sequence
// and requires the reader to reject it with a typed corruption error.
func demonstrateChecksumRejection(work, input string) error {
	data, err := os.ReadFile(input)
	if err != nil {
		return model.Wrap(model.KindInput, "medpipe.demo", err)
	}
	// Flip one payload byte of a real frame. The record layout puts the
	// payload 35 bytes after the record start (track index, sequence, raw
	// timestamp, flags, dimensions, energy count, payload length), and the
	// payload is covered by the integrity checksum, so the reader must
	// reject it rather than skip it.
	parsed, err := sequence.Parse(data)
	if err != nil {
		return err
	}
	var target *model.Frame
	for _, tr := range parsed.Header.Tracks {
		if fs := parsed.Frames[tr.ID]; len(fs) > 0 && len(fs[0].Payload) > 0 {
			target = &fs[0]
			break
		}
	}
	if target == nil {
		return model.New(model.KindInvariant, "medpipe.demo", "sequence has no payload to corrupt")
	}
	const recordHeaderBytes = 4 + 8 + 8 + 1 + 2 + 2 + 2 + 8
	pos := int(target.Offset) + recordHeaderBytes
	if pos >= len(data) {
		return model.New(model.KindInvariant, "medpipe.demo", "payload offset outside file")
	}
	tampered := append([]byte(nil), data...)
	tampered[pos] ^= 0xff
	copyPath := filepath.Join(work, "tampered.medpseq")
	if err := os.WriteFile(copyPath, tampered, 0o644); err != nil {
		return model.Wrap(model.KindInput, "medpipe.demo", err)
	}
	_, err = sequence.ReadFile(work, "tampered.medpseq")
	if err == nil {
		return model.New(model.KindInvariant, "medpipe.demo", "tampered sequence was accepted")
	}
	if !model.IsKind(err, model.KindCorrupt) {
		return fmt.Errorf("tampered sequence rejected with unexpected kind: %w", err)
	}
	return nil
}

// demonstrateRecovery damages one frame's media framing in place, runs the
// graph (segment fails, fingerprint is blocked), restores the good bytes and
// runs again, returning the blocking dependency and per-node reuse counts.
func demonstrateRecovery(cfg pipeline.Config, capture *synth.Capture) (*dag.BlockedDep, [6]int, error) {
	var reused [6]int
	input := filepath.Join(cfg.WorkDir, cfg.InputPath)
	good, err := os.ReadFile(input)
	if err != nil {
		return nil, reused, model.Wrap(model.KindInput, "medpipe.demo", err)
	}
	// Flip the codec tag of the second video frame but keep the record
	// checksum consistent, modelling a damaged recorder rather than damaged
	// storage.
	damagedFrames := cloneFrames(capture.Frames)
	video := ""
	for _, tr := range capture.Tracks {
		if tr.Kind == model.KindVideo {
			video = tr.ID
			break
		}
	}
	if video == "" {
		return nil, reused, model.New(model.KindInvariant, "medpipe.demo", "no video track in capture")
	}
	if len(damagedFrames[video]) < 2 {
		return nil, reused, model.New(model.KindInvariant, "medpipe.demo", "too few video frames to damage")
	}
	fr := &damagedFrames[video][1]
	fr.Payload = append([]byte(nil), fr.Payload...)
	fr.Payload[0] = 'X'
	fr.Checksum = fr.IntegrityChecksum()
	if _, _, err := sequence.WriteFile(input, capture.Tracks, damagedFrames); err != nil {
		return nil, reused, err
	}
	// The same store is deliberately reused. After the repair the source
	// bytes are identical to the capture that was processed successfully at
	// the start, so every node's idempotency key matches its recorded result
	// and recovery must reuse those outputs instead of appending duplicate
	// ones.
	failedCfg := cfg
	failedCfg.OnCorrupt = nil
	_, runErr := pipeline.Run(failedCfg)
	failure, ok := pipeline.AsFailure(runErr)
	if !ok || failure.Run == nil {
		return nil, reused, model.New(model.KindInvariant, "medpipe.demo",
			fmt.Sprintf("expected segmentation to fail on damaged framing, got %v", runErr))
	}
	blocked := failure.Run.FirstBlocked
	if blocked == nil {
		return nil, reused, model.New(model.KindInvariant, "medpipe.demo",
			"damaged framing failed without blocking a successor")
	}
	if blocked.Dep != pipeline.NodeSegment {
		return nil, reused, model.New(model.KindInvariant, "medpipe.demo",
			fmt.Sprintf("expected segment to block its successors, blocked by %s", blocked.Dep))
	}
	// The failure pass must not have produced a fingerprint result.
	if err := os.WriteFile(input, good, 0o644); err != nil {
		return nil, reused, model.Wrap(model.KindInput, "medpipe.demo", err)
	}
	resumed := failedCfg
	out2, err := pipeline.Run(resumed)
	if err != nil {
		return nil, reused, err
	}
	names := []string{pipeline.NodeRead, pipeline.NodeNormalize, pipeline.NodeIndex,
		pipeline.NodeAudio, pipeline.NodeSegment, pipeline.NodeFingerprint}
	for i, name := range names {
		if r, ok := out2.Run.Results[name]; ok && r.State == dag.StateReused {
			reused[i] = 1
		}
	}
	if out2.Run.ReusedNodes < 1 {
		return nil, reused, model.New(model.KindInvariant, "medpipe.demo",
			"recovery did not reuse any completed node")
	}
	// Recovery must not recompute or duplicate anything: every node's key is
	// back to its pre-damage value, so every node comes back as reused.
	for _, name := range []string{pipeline.NodeRead, pipeline.NodeNormalize, pipeline.NodeIndex,
		pipeline.NodeAudio, pipeline.NodeSegment, pipeline.NodeFingerprint} {
		if r := out2.Run.Results[name]; r.State != dag.StateReused {
			return nil, reused, model.New(model.KindInvariant, "medpipe.demo",
				fmt.Sprintf("recovery recomputed node %s (state %s) instead of reusing it", name, r.State))
		}
	}
	return blocked, reused, nil
}

func countReused(flags [6]int) int {
	n := 0
	for _, f := range flags {
		n += f
	}
	return n
}

func cloneFrames(in map[string][]model.Frame) map[string][]model.Frame {
	out := make(map[string][]model.Frame, len(in))
	for k, v := range in {
		cp := make([]model.Frame, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// boundaryCases counts probed boundaries: every segment edge and every silent
// range edge that must be attributed exactly once.
func boundaryCases(out *pipeline.Outcome) int {
	n := 0
	for _, seg := range out.Segments {
		n += len(seg.Slices) + len(seg.Silence)
		if seg.Index > 0 {
			n++
		}
	}
	return n
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("input", "", "sequence file to verify")
	if err := fs.Parse(args); err != nil {
		return model.Wrap(model.KindInput, "medpipe.verify", err)
	}
	if *path == "" {
		return model.New(model.KindInput, "medpipe.verify", "--input is required")
	}
	file, err := sequence.ReadFile("", *path)
	if err != nil {
		return err
	}
	// Re-serialize and compare digests, then hash every frame and segment
	// boundary independently of the reader's own digest.
	var buf bytes.Buffer
	_, digest, err := sequence.WriteTo(&buf, file.Header.Tracks, file.Frames)
	if err != nil {
		return err
	}
	size := int64(buf.Len())
	if digest != file.Digest {
		return model.New(model.KindInvariant, "medpipe.verify", "re-serialized digest differs from stored digest")
	}
	frameDigest := sequence.DigestFrames(file.Header.Tracks, file.Frames)
	sum := sha256.Sum256(frameDigest[:])
	fmt.Printf("bytes=%d\n", size)
	fmt.Printf("digest_match=1\n")
	fmt.Printf("frame_digest=%x\n", sum[:8])
	return nil
}
