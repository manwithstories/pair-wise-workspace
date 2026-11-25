package main

import (
	"flag"
	"fmt"
	"os"

	"avlab/internal/fixture"
	"avlab/internal/media"
	"avlab/internal/transport"
)

// runTraces generates and prints the normal, damaged and recovery network
// traces plus the damaged-segment manifest, all derived deterministically.
func runTraces(args []string) int {
	fs := flag.NewFlagSet("traces", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Uint64("seed", 17, "deterministic PRNG seed")
	packets := fs.Uint64("packets", 4096, "RTP packets in the trace")
	if err := fs.Parse(args); err != nil {
		return exitInvalid
	}
	if *packets == 0 {
		fmt.Fprintln(os.Stderr, "avlab: packets must be positive")
		return exitInvalid
	}

	tr := transport.NewTrace(*seed, *packets)
	fmt.Println("# normal trace (deterministic impairment schedule)")
	for i, st := range tr.Steps {
		fmt.Printf("step=%02d at=%d action=%s count=%d delay_tick=%d bw_kbps=%d\n",
			i, st.At, actionName(st.Action), st.Count, st.DelayTick,
			st.BandwidthKbps)
	}
	fmt.Printf("steps=%d base_bandwidth_kbps=%d\n\n", len(tr.Steps), tr.BaseKbps)

	bundle, err := fixture.Generate(*seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "avlab: fixture generation failed: %v\n", err)
		return exitGeneral
	}
	fmt.Println("# damaged inputs (each must be rejected with a classified Fault)")
	valid := bundle.Segments["mid/mid/seg-1.m4s"]
	for _, c := range fixture.BuildDamagedCases(valid) {
		fmt.Printf("case=%s expected_code=%s bytes=%d\n",
			c.Name, c.Code, len(c.Data))
	}

	fmt.Println("\n# recovery trace (simulated burst/retransmission outcome)")
	eng := transport.NewEngine(nil, transport.EngineConfig{
		Packets: *packets, Capacity: 512, WaitTick: 20, PayloadLen: 24,
	}, tr)
	res, err := eng.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "avlab: recovery run failed: %v\n", err)
		return exitGeneral
	}
	fmt.Printf("burst_drops=%d burst_recovered=%d deadline_skips=%d\n",
		res.Trace.BurstDrops, res.Jitter.BurstRecovered,
		res.Jitter.DeadlineSkips)
	fmt.Printf("duplicates=%d reordered=%d bandwidth_stairs=%d\n",
		res.Reorder.Duplicate, res.Reorder.Reordered, len(res.Trace.BwSteps))
	fmt.Printf("jitter_queue_peak=%d played=%d\n",
		res.Jitter.QueuePeak, res.Jitter.Played)
	fmt.Println("status=generated")
	return exitOK
}

func actionName(a transport.Action) string {
	switch a {
	case transport.ActPass:
		return "pass"
	case transport.ActDuplicate:
		return "duplicate"
	case transport.ActReorder:
		return "reorder"
	case transport.ActBurstDrop:
		return "burst_drop"
	case transport.ActBandwidthStep:
		return "bandwidth_step"
	default:
		return string(media.CodeInternal)
	}
}
