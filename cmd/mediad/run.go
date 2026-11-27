package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/fashionweek/mediad/internal/pipeline"
)

func runPipeline(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	job := fs.String("job", "", "path to job.json")
	work := fs.String("work", "", "working/output directory")
	interrupt := fs.Int("interrupt-after", 0,
		"synthetic stop after N completed segments (exit 75)")
	resume := fs.Bool("resume", false, "resume from an earlier checkpoint")
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitArgs, err
	}
	if *job == "" || *work == "" {
		return pipeline.ExitArgs, fmt.Errorf("run requires --job and --work")
	}
	if *resume && *interrupt != 0 {
		return pipeline.ExitArgs, fmt.Errorf("--resume cannot combine with --interrupt-after")
	}
	opts := pipeline.Options{
		JobPath: *job, WorkDir: *work,
		Resume: *resume, InterruptAfter: *interrupt,
	}
	report, code, err := pipeline.Run(context.Background(), opts)
	if err != nil {
		return code, err
	}
	if report != nil {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
		fmt.Printf("run status=%s code=%d segments=%d/%d accepted=%v rejected=%d\n",
			report.Status, code, report.SegmentsDone, report.SegmentsTotal,
			report.Accepted, len(report.Rejected))
	}
	return code, nil
}

var _ = os.Stdout
