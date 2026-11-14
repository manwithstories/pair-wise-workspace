package main

import (
	"fmt"
	"io"
	"strings"

	"rcbench/internal/job"
	"rcbench/internal/report"
)

// inspectJob prints the job manifest, legal state transitions and segments.
func inspectJob(jobDir string, w io.Writer) (int, error) {
	meta, err := job.ReadMeta(jobDir)
	if err != nil {
		return report.ExitUsage, err
	}
	state := job.State(meta.State)
	succ := state.Successors()
	readable := make([]string, len(succ))
	for i, s := range succ {
		readable[i] = string(s)
	}
	if len(readable) == 0 {
		readable = []string{"(none, terminal)"}
	}
	fmt.Fprintf(w, "job:              %s\n", jobDir)
	fmt.Fprintf(w, "input:            %s\n", meta.Input)
	fmt.Fprintf(w, "input_sha256:     %s\n", meta.InputSHA256)
	fmt.Fprintf(w, "geometry:         %dx%d @ %d fps\n", meta.Width, meta.Height, meta.FPS)
	fmt.Fprintf(w, "target_bps:       %d\n", meta.TargetBps)
	fmt.Fprintf(w, "segment_frames:   %d\n", meta.SegmentFrames)
	fmt.Fprintf(w, "total_frames:     %d\n", meta.TotalFrames)
	fmt.Fprintf(w, "segments:         %d\n", meta.Segments)
	fmt.Fprintf(w, "state:            %s\n", state)
	fmt.Fprintf(w, "legal_successors: %s\n", strings.Join(readable, ", "))
	if len(meta.Poisoned) > 0 {
		fmt.Fprintf(w, "poisoned:         %v\n", meta.Poisoned)
	}
	fmt.Fprintln(w, "segment  state       qp   bits        psnr_db")
	for seg := 1; seg <= meta.Segments; seg++ {
		ck, err := job.ReadCheckpoint(jobDir, seg)
		if err != nil {
			fmt.Fprintf(w, "%7d  incomplete\n", seg)
			continue
		}
		fmt.Fprintf(w, "%7d  done        %3d %11d  %7.2f\n", seg, ck.QP, ck.Bits, ck.PSNR)
	}
	return report.ExitOK, nil
}
