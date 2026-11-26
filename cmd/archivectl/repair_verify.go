package main

import (
	"flag"
	"fmt"
	"os"

	"archivectl/internal/core"
	"archivectl/internal/index"
	"archivectl/internal/repair"
	"archivectl/internal/verify"
	"archivectl/internal/wavscan"
)

func runRepair(args []string) int {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	indexPath := fs.String("index", "archive-index.json", "path to atomic archive index")
	wavPath := fs.String("wav", "", "override WAV path (defaults to index key path)")
	clockPath := fs.String("clock", "", "override clock records path")
	outPath := fs.String("out", "repaired.wav", "output WAV path")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	idx, err := index.Load(*indexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "repair failed: %v\n", err)
		return core.ExitCode(err)
	}
	result, err := repair.Run(repair.Options{
		Index: idx, WAVPath: *wavPath, ClockPath: *clockPath, OutputPath: *outPath,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "repair failed: %v\n", err)
		return core.ExitCode(err)
	}
	fmt.Printf("repair_status=ok\noutput=%s\nrecovered_frames=%d\nzeroed_frames=%d\ndamage_spans=%d\nsha256_output=",
		result.OutputPath, result.RecoveredFrames, result.ZeroedFrames, result.DamageSpans)
	digest, err := verify.FileSHA256(result.OutputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hash output failed: %v\n", err)
		return 1
	}
	fmt.Println(digest)
	for i, s := range result.Coverage {
		fmt.Printf("repaired_range[%d]=[%d,%d) frames=%d\n", i, s.StartFrame, s.EndFrame, s.Frames())
	}
	if result.RecoveredFrames > 0 || len(idx.Damage) > 0 {
		return 2
	}
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	repaired := fs.String("repaired", "repaired.wav", "repaired WAV to verify")
	baseline := fs.String("baseline", "", "generator baseline WAV")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *baseline == "" {
		fmt.Fprintln(os.Stderr, "verify: --baseline is required")
		return 1
	}
	gotRep, err := wavscan.ScanPath(*repaired)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
		return core.ExitCode(err)
	}
	wantRep, err := wavscan.ScanPath(*baseline)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
		return core.ExitCode(err)
	}
	actual, err := verify.HashWAVPath(*repaired, gotRep.DataOffset,
		gotRep.Format.FrameBytes(), gotRep.DeclaredFrames)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
		return core.ExitCode(err)
	}
	expected, err := verify.HashWAVPath(*baseline, wantRep.DataOffset,
		wantRep.Format.FrameBytes(), wantRep.DeclaredFrames)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
		return core.ExitCode(err)
	}
	cmp := verify.CompareSegments(actual, expected)
	fmt.Printf("sha256_segments=%d\nsha256_match=%d/%d\n", cmp.Total, cmp.Matched, cmp.Total)
	if err := cmp.AllMatch(); err != nil {
		for _, r := range cmp.Results {
			if !r.Match {
				fmt.Printf("first_mismatch=segment-%d\n", r.Index)
				break
			}
		}
		return 1
	}
	fmt.Println("verify_status=match")
	return 0
}
