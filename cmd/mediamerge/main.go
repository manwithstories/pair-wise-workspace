// Command mediamerge demonstrates lossless MP4/MOV remuxing with stage
// journaling, crash recovery and byte-precise fault reporting. It is a
// terminal-only tool: every action is driven by flags and printed lines.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mediamerge/internal/model"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "corrupt":
		err = runCorrupt(os.Args[2:])
	case "recover":
		err = runRecover(os.Args[2:])
	case "merge":
		err = runMerge(os.Args[2:])
	case "plan":
		err = runPlan(os.Args[2:])
	case "summarize":
		err = runSummarize(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		var pe *model.Error
		if errors.As(err, &pe) {
			fmt.Fprintf(os.Stderr, "%v\n", pe)
			if pe.Code == model.CodeStream || pe.Code == model.CodeWrite ||
				pe.Code == model.CodeCanceled || pe.Code == model.CodeConsumerGone {
				if b := pe.Boundary; b != nil {
					fmt.Fprintf(os.Stderr, "last_complete_boundary sample=%d track=%d output_bytes=%d\n",
						b.SampleIndex, b.TrackID, b.OutputBytes)
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(model.ExitCode(err))
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediamerge - lossless MP4/MOV remux with recoverable stage commits

usage:
  mediamerge demo    --dir <workdir> [--tracks N] [--samples N] [--queue N]
  mediamerge verify  --dir <workdir>
  mediamerge corrupt --dir <workdir> [--kind truncated-box|corrupt-sample-table|tail-truncation|corrupt-sample]
  mediamerge recover --dir <workdir> [--interrupt-after N] [--queue N]
  mediamerge merge   --input <file> --output <file> [--map "1=10:video,2=11:audio"]
  mediamerge plan      --input <file> [--map ...]
  mediamerge summarize --dir <workdir> [--json]
`)
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func requireDir(fs *flag.FlagSet, dir string) error {
	if dir == "" {
		fs.Usage()
		return model.NewError(model.StageVerify, model.CodePath, -1, 0, false, "--dir is required")
	}
	return nil
}
