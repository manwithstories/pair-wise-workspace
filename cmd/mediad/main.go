package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mediad/internal/bmff"
	"mediad/internal/model"
)

func main() {
	os.Exit(runCLI(os.Args[1:]))
}

func runCLI(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "scan":
		return cmdScan(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "version":
		fmt.Println("mediad 1.0.0")
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	work := fs.String("work", "", "demo working directory")
	seed := fs.Int64("seed", 17, "deterministic generator seed")
	profile := fs.String("profile", "variable", "network profile: variable or stable")
	interruptAfter := fs.Int("interrupt-after", 2, "committed samples before simulated interruption")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected positional arguments: %v\n", fs.Args())
		return 2
	}
	err := runDemo(demoConfig{
		Work: *work, Seed: *seed, Profile: *profile, InterruptAfter: *interruptAfter,
	})
	if err != nil {
		printError(err)
		return model.ExitCode(err)
	}
	return 0
}

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	input := fs.String("input", "", "path to MP4/MOV input")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	path := *input
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	if path == "" || fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: mediad scan --input <file.mp4>")
		return 2
	}
	idx, err := bmff.ScanFile(path)
	if err != nil {
		printError(err)
		return model.ExitCode(err)
	}
	samples := 0
	for _, t := range idx.Tracks {
		samples += len(t.Samples)
	}
	fmt.Printf("input_sha256=%s\n", idx.InputSHA256)
	fmt.Printf("fragmented=%t\n", idx.Fragmented)
	fmt.Printf("tracks=%d\n", len(idx.Tracks))
	fmt.Printf("samples=%d\n", samples)
	for _, t := range idx.Tracks {
		fmt.Printf("track=%d codec=%s handler=%s timescale=%d samples=%d encrypted=%t\n",
			t.ID, t.Codec, t.Handler, t.Timescale, len(t.Samples), t.Encrypted)
	}
	return 0
}

func printError(err error) {
	fmt.Fprintf(os.Stderr, "mediad: %v\n", err)
	var me *model.Error
	if errors.As(err, &me) {
		fmt.Fprintf(os.Stderr, "code=%s stage=%s exit=%d", me.Code, me.Stage, model.ExitCode(me))
		if me.HasOffset {
			fmt.Fprintf(os.Stderr, " offset=%d", me.Offset)
		}
		if me.BoxType != "" {
			fmt.Fprintf(os.Stderr, " box=%s", me.BoxType)
		}
		fmt.Fprintln(os.Stderr)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: mediad <demo|scan|verify|version> [flags]")
	fmt.Fprintln(w, "  demo --work DIR --seed N --profile variable|stable --interrupt-after N")
	fmt.Fprintln(w, "  scan --input FILE.mp4")
	fmt.Fprintln(w, "  verify --checkpoint FILE.json [--output FILE.mp4]")
}
