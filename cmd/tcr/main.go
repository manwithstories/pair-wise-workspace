// tcr is a terminal-only segmented transcoder with per-segment rate
// control, checkpointed resume, PSNR metrics and baseline regression.
//
// Exit codes: 0 ok, 1 parameter/usage error, 2 quality regression,
// 3 infeasible bitrate, 4 corrupt state, 5 injected failure.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"tcr/internal/codec"
	"tcr/internal/fixture"
	"tcr/internal/metrics"
	"tcr/internal/rc"
	"tcr/internal/report"
	"tcr/internal/sched"
	"tcr/internal/task"
	"tcr/internal/yuv"
)

func main() { os.Exit(run(os.Args[1:])) }

func fail(code int, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	return code
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	switch args[0] {
	case "fixture":
		return cmdFixture(args[1:])
	case "encode":
		return cmdEncode(args[1:])
	case "metrics":
		return cmdMetrics(args[1:])
	case "regress":
		return cmdRegress(args[1:])
	default:
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  tcr fixture --out DIR --segs N --w W --h H --frames F [--fps N]
  tcr encode --manifest M --target-kbps K --out DIR [--workers N] [--fail-after-segs N] [--resume STATE]
  tcr metrics --manifest M --dec DIR --json FILE
  tcr regress --baseline A --current B --max-psnr-drop D --max-seg-std S`)
}

func cmdFixture(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	segs := fs.Int("segs", 6, "number of segments")
	w := fs.Int("w", 320, "width")
	h := fs.Int("h", 240, "height")
	frames := fs.Int("frames", 48, "total frames")
	fps := fs.Int("fps", 25, "frames per second")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *out == "" {
		return fail(1, "--out is required")
	}
	m, err := fixture.Generate(*out, *segs, *w, *h, *frames, *fps)
	if err != nil {
		return fail(1, "%v", err)
	}
	fmt.Printf("fixture: %d segments, %dx%d, %d frames, manifest at %s\n",
		len(m.Segments), m.Width, m.Height, m.Frames, filepath.Join(*out, "manifest.json"))
	return 0
}

func cmdEncode(args []string) int {
	fs := flag.NewFlagSet("encode", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "manifest JSON")
	target := fs.Int("target-kbps", 0, "target bitrate in kbps")
	out := fs.String("out", "", "output directory")
	workers := fs.Int("workers", 2, "concurrent segment workers")
	failAfter := fs.Int("fail-after-segs", -1, "inject failure after N completed segments")
	resume := fs.String("resume", "", "resume from state file")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *manifestPath == "" || *target <= 0 || (*out == "" && *resume == "") {
		return fail(1, "encode requires --manifest, --target-kbps > 0, and --out (or --resume)")
	}
	m, err := fixture.LoadManifest(*manifestPath)
	if err != nil {
		return fail(1, "%v", err)
	}

	var st *task.State
	statePath := filepath.Join(*out, "state.json")
	if *resume != "" {
		statePath = *resume
		st, err = task.Load(statePath)
		if err != nil {
			return fail(4, "%v", err)
		}
		if len(st.Segments) != len(m.Segments) || st.TargetKbps != *target {
			return fail(4, "%v: state does not match manifest/target", task.ErrCorrupt)
		}
		if *out == "" {
			*out = filepath.Dir(statePath)
		}
	} else {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return fail(1, "%v", err)
		}
		st = task.New(*manifestPath, *target, len(m.Segments))
		if err := st.Save(statePath); err != nil {
			return fail(1, "cannot write state: %v", err)
		}
	}

	pending := st.PendingIndices()
	if len(pending) == 0 {
		fmt.Println("encode: all segments already done")
		return 0
	}
	masterDir := filepath.Dir(*manifestPath)
	type segOut struct {
		qpEff float64
		bits  int64
	}
	work := func(seg int) (segOut, error) {
		s := m.Segments[seg]
		frames, err := yuv.LoadSegment(filepath.Join(masterDir, s.File), m.Width, m.Height)
		if err != nil {
			return segOut{}, err
		}
		hdr := codec.SegmentHeader{SegIndex: seg, FrameCount: len(frames), W: m.Width, H: m.Height, FPS: m.FPS, TargetKbps: *target}
		targetBits := int64(*target) * 1000 * int64(len(frames)) / int64(m.FPS)
		oc, err := rc.EncodeSegment(frames, hdr, targetBits, filepath.Join(*out, codec.SegmentFileName(seg)))
		if err != nil {
			return segOut{}, err
		}
		return segOut{qpEff: oc.QPEff, bits: oc.Bits}, nil
	}
	onDone := func(seg int, v segOut) {
		if err := st.MarkDone(seg, v.qpEff, v.bits, codec.SegmentFileName(seg), statePath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: checkpoint write failed: %v\n", err)
		}
		fmt.Printf("checkpoint: seg %d done (qp=%.1f bits=%d)\n", seg, v.qpEff, v.bits)
	}
	runErr := sched.Run(pending, *workers, *failAfter, work, onDone)
	if runErr != nil {
		var infeasible *rc.InfeasibleError
		if errors.As(runErr, &infeasible) {
			fmt.Fprintf(os.Stderr, "%v\nsuggested target: >= %.0f kbps\n", infeasible, infeasible.MinKbps)
			return 3
		}
		if errors.Is(runErr, sched.ErrInjected) {
			fmt.Fprintf(os.Stderr, "%v; resume with: tcr encode --manifest %s --target-kbps %d --resume %s\n",
				runErr, *manifestPath, *target, statePath)
			return 5
		}
		return fail(1, "%v", runErr)
	}
	fmt.Printf("encode: %d segments complete, state at %s\n", len(m.Segments), statePath)
	return 0
}

func cmdMetrics(args []string) int {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "manifest JSON")
	decDir := fs.String("dec", "", "directory with encoded segments")
	jsonPath := fs.String("json", "", "output metrics JSON")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *manifestPath == "" || *decDir == "" || *jsonPath == "" {
		return fail(1, "metrics requires --manifest, --dec and --json")
	}
	m, err := fixture.LoadManifest(*manifestPath)
	if err != nil {
		return fail(1, "%v", err)
	}
	masterDir := filepath.Dir(*manifestPath)
	var segs []report.SegReport
	var psnrs []float64
	var totalBits int64
	targetKbps := 0
	for _, s := range m.Segments {
		encPath := filepath.Join(*decDir, codec.SegmentFileName(s.Index))
		dec, err := codec.ReadSegmentFile(encPath)
		if err != nil {
			return fail(1, "%v", err)
		}
		orig, err := yuv.LoadSegment(filepath.Join(masterDir, s.File), m.Width, m.Height)
		if err != nil {
			return fail(1, "%v", err)
		}
		psnr, err := metrics.SegmentPSNR(orig, dec.Frames)
		if err != nil {
			return fail(1, "segment %d: %v", s.Index, err)
		}
		info, err := os.Stat(encPath)
		if err != nil {
			return fail(1, "%v", err)
		}
		bits := info.Size() * 8
		totalBits += bits
		targetKbps = dec.Header.TargetKbps
		qpEff := 0.0
		for _, q := range dec.QPs {
			qpEff += float64(q)
		}
		qpEff /= float64(len(dec.QPs))
		segs = append(segs, report.SegReport{Index: s.Index, Name: s.Name, Frames: s.Frames, QP: qpEff, Bits: bits, PSNRDb: psnr})
		psnrs = append(psnrs, psnr)
	}
	seconds := float64(m.Frames) / float64(m.FPS)
	actualKbps := float64(totalBits) / seconds / 1000
	dev := 0.0
	if targetKbps > 0 {
		dev = (actualKbps - float64(targetKbps)) / float64(targetKbps) * 100
		if dev < 0 {
			dev = -dev
		}
	}
	mean, std := metrics.MeanStd(psnrs)
	rep := report.New(targetKbps, actualKbps, dev, mean, std, segs)
	if err := rep.Save(*jsonPath); err != nil {
		return fail(1, "cannot write metrics: %v", err)
	}
	fmt.Println(rep.Summary())
	fmt.Printf("metrics written to %s\n", *jsonPath)
	return 0
}

func cmdRegress(args []string) int {
	fs := flag.NewFlagSet("regress", flag.ContinueOnError)
	basePath := fs.String("baseline", "", "baseline metrics JSON")
	curPath := fs.String("current", "", "current metrics JSON")
	maxDrop := fs.Float64("max-psnr-drop", 0.5, "max allowed mean PSNR drop in dB")
	maxStd := fs.Float64("max-seg-std", 2.0, "max allowed segment PSNR stddev in dB")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *basePath == "" || *curPath == "" {
		return fail(1, "regress requires --baseline and --current")
	}
	base, err := report.Load(*basePath)
	if err != nil {
		return fail(1, "%v", err)
	}
	cur, err := report.Load(*curPath)
	if err != nil {
		return fail(1, "%v", err)
	}
	ok, reasons := report.Regress(base, cur, *maxDrop, *maxStd)
	if !ok {
		for _, r := range reasons {
			fmt.Fprintf(os.Stderr, "regression: %s\n", r)
		}
		return 2
	}
	fmt.Printf("regress: pass (psnr_mean %.2f -> %.2f dB, seg_std %.2f dB)\n",
		base.PSNRMeanDb, cur.PSNRMeanDb, cur.PSNRSegStdDb)
	return 0
}
