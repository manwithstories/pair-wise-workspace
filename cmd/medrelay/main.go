// Command medrelay is the terminal acceptance adapter: it generates local
// HLS/DASH + RTP samples, runs the media pipeline under a WAL-backed
// transaction and prints a verification summary. Exit codes:
// 0 ok, 2 input/argument error, 3 media corruption, 4 txn/digest failure.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"medrelay/internal/journal"
	"medrelay/internal/timeline"
	"medrelay/internal/txn"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "resume":
		return cmdResume(args[1:])
	default:
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  medrelay demo   [--loss-burst N] [--corrupt-segment] [--interrupt-after STAGE] [--workdir DIR]
  medrelay resume --verify [--workdir DIR]
stages: manifest transport normalize split silence fingerprint report`)
}

// timedStages wraps each stage with wall-clock measurement.
func timedStages(stages []txn.Stage, times *[]float64) []txn.Stage {
	out := make([]txn.Stage, len(stages))
	for i, st := range stages {
		st := st
		out[i] = txn.Stage{Name: st.Name, Run: func(dir string) error {
			t0 := time.Now()
			err := st.Run(dir)
			*times = append(*times, float64(time.Since(t0).Microseconds())/1000)
			return err
		}}
	}
	return out
}

func printSummary(r *txn.Runner, times []float64, sum *summaryOut, outputSHA string) {
	if sum != nil {
		fmt.Printf("av_sync_error_ms=%.2f\n", sum.AVSyncErrorMs)
		fmt.Printf("packet_recovery_rate=%.4f\n", sum.PacketRecoveryRate)
		fmt.Printf("first_error_segment=%s\n", sum.FirstErrorSegment)
	}
	fmt.Printf("pipeline_p95_ms=%.1f\n", p95(times))
	fmt.Printf("replayed_stages=%s\n", strings.Join(r.Replayed, ","))
	fmt.Printf("rollback_count=%d\n", r.Rollbacks)
	fmt.Printf("output_sha256=%s\n", outputSHA)
}

// mapStageErr converts a stage error to an exit code.
func mapStageErr(err error) int {
	var te *timeline.Error
	if errors.As(err, &te) {
		switch te.Code {
		case timeline.ErrLimit, timeline.ErrMissingRef, timeline.ErrInvalidDur,
			timeline.ErrDuplicateSeq, timeline.ErrTimestampRange:
			fmt.Fprintln(os.Stderr, "input error:", te)
			return 2
		}
	}
	fmt.Fprintln(os.Stderr, "pipeline error:", err)
	return 4
}

func openRunner(workdir, interruptAfter string) (*txn.Runner, error) {
	w, err := journal.Open(filepath.Join(workdir, "wal.log"), 512)
	if err != nil {
		return nil, err
	}
	return &txn.Runner{
		WAL:            w,
		Root:           filepath.Join(workdir, "stages"),
		InterruptAfter: interruptAfter,
	}, nil
}

func readSummary(workdir string) *summaryOut {
	var sum summaryOut
	if err := readJSON(stageDir(workdir, "report"), "summary.json", &sum); err != nil {
		return nil
	}
	return &sum
}

func reportDigest(workdir string) string {
	d, err := txn.DigestDir(stageDir(workdir, "report"))
	if err != nil {
		return "unavailable"
	}
	return d
}

// partialSummary builds a summary from whatever stages completed, so an
// interrupted run still reports the first corrupt segment.
func partialSummary(workdir string) *summaryOut {
	if sum := readSummary(workdir); sum != nil {
		return sum
	}
	var quar quarantineOut
	if err := readJSON(stageDir(workdir, "manifest"), "quarantine.json", &quar); err != nil {
		return nil
	}
	sum := &summaryOut{}
	for _, q := range quar.Items {
		if sum.FirstErrorSegment == "" {
			sum.FirstErrorSegment = q.Segment
		}
		sum.Quarantined = append(sum.Quarantined, q.Segment)
	}
	return sum
}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	lossBurst := fs.Int("loss-burst", 4, "consecutive RTP packets to drop")
	corrupt := fs.Bool("corrupt-segment", false, "truncate segment 3")
	interruptAfter := fs.String("interrupt-after", "", "crash before committing this stage")
	workdir := fs.String("workdir", ".medrelay", "working directory (inside workspace)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *lossBurst < 0 || *lossBurst > redStride {
		fmt.Fprintf(os.Stderr, "loss-burst must be in [0,%d]\n", redStride)
		return 2
	}
	validStage := map[string]bool{"": true, "manifest": true, "transport": true,
		"normalize": true, "split": true, "silence": true, "fingerprint": true, "report": true}
	if !validStage[*interruptAfter] {
		fmt.Fprintln(os.Stderr, "unknown stage:", *interruptAfter)
		return 2
	}
	if filepath.IsAbs(*workdir) || strings.HasPrefix(*workdir, "..") {
		fmt.Fprintln(os.Stderr, "workdir must stay inside the workspace")
		return 2
	}
	if err := os.RemoveAll(*workdir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	if err := generateSample(*workdir, *lossBurst, *corrupt); err != nil {
		fmt.Fprintln(os.Stderr, "sample generation:", err)
		return 4
	}
	r, err := openRunner(*workdir, *interruptAfter)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	defer r.WAL.Close()
	var times []float64
	stages := timedStages(pipelineStages(*workdir), &times)
	runErr := r.Execute(stages)
	if errors.Is(runErr, txn.ErrInterrupt) {
		fmt.Printf("interrupted_after=%s\n", *interruptAfter)
		sum := partialSummary(*workdir)
		printSummary(r, times, sum, "unavailable")
		if sum != nil && sum.FirstErrorSegment != "" {
			return 3
		}
		return 0
	}
	if runErr != nil {
		return mapStageErr(runErr)
	}
	sum := readSummary(*workdir)
	printSummary(r, times, sum, reportDigest(*workdir))
	if sum != nil && sum.FirstErrorSegment != "" {
		return 3
	}
	return 0
}

func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	verify := fs.Bool("verify", false, "verify committed digests after replay")
	workdir := fs.String("workdir", ".medrelay", "working directory (inside workspace)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*verify {
		fmt.Fprintln(os.Stderr, "resume requires --verify")
		return 2
	}
	if _, err := os.Stat(filepath.Join(*workdir, "wal.log")); err != nil {
		fmt.Fprintln(os.Stderr, "no prior run found in", *workdir)
		return 2
	}
	r, err := openRunner(*workdir, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	defer r.WAL.Close()
	var times []float64
	stages := timedStages(pipelineStages(*workdir), &times)
	if err := r.Execute(stages); err != nil {
		return mapStageErr(err)
	}
	if err := r.Verify(pipelineStages(*workdir)); err != nil {
		fmt.Fprintln(os.Stderr, "verify failed:", err)
		return 4
	}
	sum := readSummary(*workdir)
	printSummary(r, times, sum, reportDigest(*workdir))
	fmt.Println("verify=ok")
	return 0
}
