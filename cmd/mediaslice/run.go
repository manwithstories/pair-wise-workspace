package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"mediaslice/media"
	"mediaslice/scan"
	"mediaslice/stream"
)

// runPipeline executes the tool against an existing source manifest.
func runPipeline(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "path to source manifest JSON")
	workDir := fs.String("work-dir", "mediaslice-out", "output working directory")
	segmentSecs := fs.Float64("segment-seconds", 2, "target segment duration in seconds")
	if err := fs.Parse(args); err != nil {
		return exitInternal
	}
	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "parameter error: --manifest is required")
		return exitInternal
	}
	m, err := scan.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parameter error: %v\n", err)
		return exitInternal
	}
	segmentTicks := media.FromSeconds(*segmentSecs)
	inputs := make([]pipelineInputs, 0, len(m.Sources))
	for _, s := range m.Sources {
		inputs = append(inputs, pipelineInputs{id: s.ID, mp4Path: s.MP4Path, rtpPath: s.RTPPath})
	}
	built, err := buildPlan(*workDir, inputs, segmentTicks, segmentTicks)
	if err != nil {
		return classifyError(err)
	}
	if built.plan == nil {
		fmt.Fprintln(os.Stderr, "corrupt input: no sliceable representation")
		return exitCorrupt
	}
	if err := executePlan(*workDir, built, stream.DefaultConfig(), false, 10); err != nil {
		return classifyError(err)
	}
	out, _ := json.Marshal(map[string]string{
		"manifest":     "manifest.json",
		"output_sha256": built.runnerResult.OutputSHA256,
	})
	fmt.Println(string(out))
	return exitOK
}

func scanFileLight(path string) (*scan.FileIndex, error) {
	return scan.ScanFile(path, "", scan.DefaultConfig())
}
