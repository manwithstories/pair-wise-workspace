package main

import (
	"fmt"
	"os"
)

func main() {
	o, err := parseArgs(os.Args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediascan:", err)
		os.Exit(exitCorruptInput)
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mediascan: output dir:", err)
		os.Exit(exitIndexFault)
	}
	rep, code := Run(o)
	printReport(o, rep, code)
	os.Exit(code)
}

// printReport emits the stable key=value acceptance contract.
func printReport(o Options, r Report, code int) {
	fmt.Printf("frames=%d\n", r.Frames)
	fmt.Printf("gop_p50=%d\n", r.GOPp50)
	fmt.Printf("duplicates=%d\n", r.Dup)
	fmt.Printf("reordered=%d\n", r.Reordered)
	fmt.Printf("burst_loss=%d\n", r.BurstLoss)
	fmt.Printf("bitrate_target_kbps=%d\n", r.TargetKbps)
	fmt.Printf("bitrate_actual_kbps=%d\n", r.ActualKbps)
	fmt.Printf("bitrate_error_pct=%.2f\n", float64(r.ErrorPctX10)/100)
	fmt.Printf("quality_valid_pct=%.2f\n", float64(r.ValidPctX100)/100)
	fmt.Printf("manifest_alignment_ms=%d\n", r.AlignmentMS)
	fmt.Printf("corrupt_ranges=%d\n", r.CorruptRanges)
	fmt.Printf("fatal_faults=%d\n", r.FatalFaults)
	fmt.Printf("checkpoint_chunks=%d\n", r.CheckpointChunks)
	if r.RecoveredChunks > 0 {
		fmt.Printf("recovered_chunks=%d\n", r.RecoveredChunks)
	}
	fmt.Printf("index_first_tick=%d\n", r.FirstTick)
	fmt.Printf("index_last_tick=%d\n", r.LastTick)
	fmt.Printf("input_digest=%s\n", r.InputDigest)
	fmt.Printf("exit_code=%d\n", code)
	switch code {
	case exitOK:
		fmt.Println("status=completed")
	case exitCorruptInput:
		fmt.Println("status=corrupt_input")
	case exitIndexFault:
		fmt.Println("status=index_fault")
	case exitInterrupted:
		fmt.Println("status=interrupted_resumable")
	}
}
