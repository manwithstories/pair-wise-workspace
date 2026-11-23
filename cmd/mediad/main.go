// Command mediad is the acceptance entry point for the rapid post
// delivery pipeline.
package main

import (
	"fmt"
	"os"

	"github.com/example/mediad/internal/model"
	"github.com/example/mediad/internal/pipeline"
	"github.com/example/mediad/internal/txn"
)

// Exit codes per the acceptance contract.
const (
	exitOK       = 0
	exitFailure  = 1
	exitArgument = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitArgument
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage()
		return exitArgument
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr,
		"usage: mediad demo --seed N --loss N [--crash-after write|verify|commit] [--work-dir DIR]\n"+
			"       mediad verify --manifest PATH")
}

type demoFlags struct {
	seed       int64
	loss       int
	crashAfter string
	workDir    string
}

func parseDemoFlags(args []string) (demoFlags, error) {
	f := demoFlags{seed: 41, loss: 3, workDir: "artifacts-run"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		need := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch a {
		case "--seed":
			v, err := need()
			if err != nil {
				return f, err
			}
			_, err = fmt.Sscan(v, &f.seed)
		case "--loss":
			v, err2 := need()
			if err2 != nil {
				return f, err2
			}
			_, err = fmt.Sscan(v, &f.loss)
		case "--crash-after":
			f.crashAfter, err = need()
		case "--work-dir":
			f.workDir, err = need()
		default:
			err = fmt.Errorf("unknown demo flag %q", a)
		}
		if err != nil {
			return f, err
		}
	}
	switch f.crashAfter {
	case "", string(txn.CrashAfterWrite), string(txn.CrashAfterVerify),
		string(txn.CrashBeforeCommit):
	default:
		return f, fmt.Errorf("invalid --crash-after %q", f.crashAfter)
	}
	if f.loss < 0 {
		return f, fmt.Errorf("--loss must be non-negative")
	}
	return f, nil
}

func runDemo(args []string) int {
	flags, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
		return exitArgument
	}
	res, err := pipeline.Run(pipeline.Options{
		WorkDir:      flags.workDir,
		Seed:         flags.seed,
		Loss:         flags.loss,
		CrashAfter:   flags.crashAfter,
		VideoSamples: 12,
	})
	if err != nil {
		if me, ok := model.AsError(err); ok && me.Kind == model.KindArgument {
			fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
			return exitArgument
		}
		fmt.Fprintf(os.Stderr, "processing failed: %v\n", err)
		return exitFailure
	}
	fmt.Println("mediad demo complete")
	fmt.Println(res.Metrics.ReportLine())
	fmt.Printf("manifest=%s\n", flags.workDir+"/artifacts/manifest.json")
	for _, rec := range res.Rejected {
		fmt.Printf("quarantined: %s reason=%s\n", rec.ID, rec.RejectReason)
	}
	return exitOK
}

func runVerify(args []string) int {
	manifest := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--manifest" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--manifest needs a value")
				return exitArgument
			}
			manifest = args[i+1]
			i++
			continue
		}
		fmt.Fprintf(os.Stderr, "unknown verify flag %q\n", args[i])
		return exitArgument
	}
	if manifest == "" {
		fmt.Fprintln(os.Stderr, "--manifest is required")
		return exitArgument
	}
	report, err := pipeline.VerifyManifest(manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification failed: %v\n", err)
		if report != nil {
			fmt.Println(report.ReportLine())
		}
		return exitFailure
	}
	fmt.Println("mediad verify complete")
	fmt.Println(report.ReportLine())
	return exitOK
}
