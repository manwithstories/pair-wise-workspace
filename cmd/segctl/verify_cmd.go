package main

import (
	"encoding/json"
	"flag"
	"os"

	"segctl/internal/verify"
)

func runVerify(args []string) (int, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	master := fs.String("master", "testdata/index.m3u8", "master playlist to verify")
	root := fs.String("root", "testdata/slices", "sliced segment root")
	jsonOut := fs.String("json", "", "optional JSON report output path")
	thresholdMs := fs.Int64("threshold-ms", 200, "max |duration deviation| vs base rendition")
	bitrateTol := fs.Float64("bitrate-tol", 0.15, "allowed relative bitrate deviation")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}

	rep, err := verify.Run(verify.Options{
		MasterPath:  *master,
		Root:        *root,
		ThresholdMs: *thresholdMs,
		BitrateTol:  *bitrateTol,
	})
	if err != nil {
		return 1, err
	}
	verify.SortFailures(rep.Failures)
	if *jsonOut != "" {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return 1, err
		}
		if err := writeFileAtomic(*jsonOut, data); err != nil {
			return 1, err
		}
	}
	rep.WriteTable(os.Stdout)
	return rep.ExitCode(), nil
}
