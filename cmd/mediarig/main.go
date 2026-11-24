package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"mediarig/internal/fixture"
	"mediarig/internal/media"
	"mediarig/internal/pipeline"
	"mediarig/internal/report"
	"mediarig/internal/txn"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(media.ExitInternal)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "run":
		err = runBatch(os.Args[2:])
	case "resume":
		err = resumeBatch(os.Args[2:])
	case "audit":
		err = auditBatch(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stdout, "exit_code=2 error_stage=cli message=unknown_command_%s committed=false commit=false\n", os.Args[1])
		os.Exit(media.ExitCorrupt)
	}
	if err != nil {
		printError(err)
		os.Exit(media.ExitCode(err))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mediarig <demo|run|resume|audit> [flags]")
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	root := fs.String("root", "", "batch root")
	caseName := fs.String("case", "clean", "fixture case: clean|damaged|truncated")
	rate := fs.Int("rate", 48000, "input audio sample rate")
	layout := fs.String("layout", "stereo", "input channel layout")
	lufs := fs.Float64("lufs", -23, "target integrated loudness")
	targetKbps := fs.Float64("target-kbps", 1536, "target PCM bitrate")
	drop := fs.Int("drop", 0, "number of isolated packets to drop")
	reorder := fs.Int("reorder", 0, "number of adjacent reorder events")
	wrapSeq := fs.Bool("wrap-seq", false, "wrap RTP sequence numbers")
	wrapTS := fs.Bool("wrap-ts", false, "wrap RTP timestamps")
	colorConflict := fs.Bool("color-conflict", false, "generate conflicting color metadata")
	truncateLast := fs.Bool("truncate-last", false, "truncate final record by one byte")
	if err := fs.Parse(args); err != nil {
		return media.Corrupt(media.StageInput, "parse demo flags: %v", err)
	}
	if *root == "" {
		return media.Corrupt(media.StageInput, "--root is required")
	}
	if *caseName == "truncated" {
		*truncateLast = true
	}
	opts := fixture.DefaultOptions(*root)
	opts.Rate = *rate
	opts.Layout = *layout
	opts.LUFS = *lufs
	opts.TargetKbps = *targetKbps
	opts.Drop = *drop
	opts.Reorder = *reorder
	opts.WrapSeq = *wrapSeq
	opts.WrapTS = *wrapTS
	opts.ColorConflict = *colorConflict
	opts.TruncateLast = *truncateLast
	if _, err := fixture.Generate(opts); err != nil {
		return err
	}
	params := txn.Params{
		Case: *caseName, Root: *root, Rate: *rate, Layout: *layout, LUFS: *lufs,
		TargetKbps: *targetKbps, Drop: *drop, Reorder: *reorder, WrapSeq: *wrapSeq,
		WrapTS: *wrapTS, ColorConflict: *colorConflict, TruncateLast: *truncateLast,
	}
	res, err := pipeline.RunFresh(params)
	if err != nil {
		return err
	}
	fmt.Println(res.Stats.KV())
	return nil
}

func runBatch(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	root := fs.String("root", "", "batch root")
	failAfter := fs.String("fail-after", "", "stage after which to inject recoverable interruption")
	if err := fs.Parse(args); err != nil {
		return media.Corrupt(media.StageInput, "parse run flags: %v", err)
	}
	if *root == "" {
		return media.Corrupt(media.StageInput, "--root is required")
	}
	p, err := txn.Layout(*root)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.InputFile); os.IsNotExist(err) {
		opts := fixture.DefaultOptions(*root)
		if _, err := fixture.Generate(opts); err != nil {
			return err
		}
	} else if err != nil {
		return media.InternalWrap(media.StageInput, err, "stat input")
	}
	params := txn.Params{
		Case: "recovery", Root: *root, Rate: 48000, Layout: "stereo", LUFS: -23,
		TargetKbps: 1536, FailAfter: *failAfter,
	}
	res, err := pipeline.RunFresh(params)
	if err != nil {
		return err
	}
	fmt.Println(res.Stats.KV())
	return nil
}

func resumeBatch(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	root := fs.String("root", "", "batch root")
	if err := fs.Parse(args); err != nil {
		return media.Corrupt(media.StageInput, "parse resume flags: %v", err)
	}
	if *root == "" {
		return media.Corrupt(media.StageInput, "--root is required")
	}
	res, err := pipeline.Resume(*root)
	if err != nil {
		return err
	}
	if res.ArchivedLine != "" {
		fmt.Println(res.ArchivedLine)
	} else {
		fmt.Println(res.Stats.KV())
	}
	return nil
}

func auditBatch(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	root := fs.String("root", "", "batch root")
	if err := fs.Parse(args); err != nil {
		return media.Corrupt(media.StageInput, "parse audit flags: %v", err)
	}
	if *root == "" {
		return media.Corrupt(media.StageInput, "--root is required")
	}
	line, err := pipeline.Audit(*root)
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}

func printError(err error) {
	re, ok := media.AsRigError(err)
	if !ok {
		re = media.Internal(media.StageInput, "%v", err)
	}
	stage := string(re.Stage)
	if stage == "" {
		stage = "internal"
	}
	line := report.ErrorLine{
		ErrorStage: stage,
		Committed:  false,
		Commit:     "false",
		ExitCode:   re.Code,
		Message:    err.Error(),
	}
	if re.PacketSequence != nil {
		line.PacketSequence = re.PacketSequence
	}
	if re.RecordIndex != nil {
		line.RecordIndex = re.RecordIndex
	}
	// Ensure message cannot break key=value parsing.
	line.Message = strings.ReplaceAll(line.Message, "\n", " ")
	fmt.Println(line.KV())
}
