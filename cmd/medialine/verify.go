package main

import (
	"fmt"
	"os"

	"medialine/internal/mp4"
	"medialine/internal/verify"
)

// runVerify re-checks every delivered fragment under --out.
func runVerify(args []string) int {
	out, err := parseVerifyFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "medialine verify: %v\n", err)
		return exitFatal
	}
	rep, err := verify.Run(out, mp4.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "medialine verify: %v\n", err)
		return exitFatal
	}
	for _, issue := range rep.Issues {
		fmt.Fprintf(os.Stderr, "manifest issue: %s\n", issue)
	}
	for _, r := range rep.Results {
		if r.OK {
			continue
		}
		fmt.Fprintf(os.Stderr, "FAILED %s: %v\n", r.ID, r.Err)
	}
	fmt.Printf("verified=%d/%d roundtrip=%d skipped=%d failed=%d match_rate=%.3f\n",
		rep.Verified, rep.Completed, rep.RoundTripped, rep.Skipped, rep.Failed, rep.MatchRate())
	if rep.Failed != 0 {
		return exitFatal
	}
	return exitOK
}
