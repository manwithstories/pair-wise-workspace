package main

import (
	"flag"
	"fmt"
	"os"

	"archivectl/internal/synth"
)

func runSynth(args []string) int {
	fs := flag.NewFlagSet("synth", flag.ContinueOnError)
	out := fs.String("out", "testdata", "output directory for generated fixtures")
	seed := fs.Int64("seed", 41, "deterministic generation seed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	files, err := synth.Generate(*out, *seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "synth failed: %v\n", err)
		return 1
	}
	fmt.Printf("seed=%d\n", *seed)
	fmt.Printf("normal=%s\n", files.Normal)
	fmt.Printf("fragmented=%s\n", files.Fragmented)
	fmt.Printf("wrap_rtp=%s\n", files.WrapRTP)
	fmt.Printf("burst_rtp=%s\n", files.BurstRTP)
	fmt.Printf("truncated_mp4=%s\n", files.TruncatedMP4)
	return 0
}
