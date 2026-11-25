package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mediad/internal/ingest"
	"mediad/internal/mediaerr"
)

func runScanCommand(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("file", "", "local MPEG-TS file to scan")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if *path == "" || fs.NArg() != 0 {
		return usageError("scan requires exactly --file PATH")
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return operationalError(err)
	}
	records, report, scanErr := ingest.ScanBytes(data)
	if report == nil {
		return pipelineError(scanErr)
	}
	fmt.Printf("file=%s size=%d packets=%d records=%d first_bad_offset=%d cc_gaps=%d psi_rejected=%d truncated_offset=%d trailing_bytes=%d\n",
		*path, len(data), report.PacketCount, len(records), report.FirstBadOffset,
		len(report.Gaps), report.PSIRejected, report.TruncatedOffset, report.TrailingBytes)
	for _, g := range report.Gaps {
		fmt.Printf("cc_gap offset=%d pid=%d expected=%d got=%d missing=%d\n",
			g.Offset, g.PID, g.Expected, g.Got, g.Missing)
	}
	for _, bad := range report.BadPackets {
		fmt.Printf("bad_packet offset=%d pid=%d error=%v\n", bad.Offset, bad.PID, bad.Reason)
		var se *mediaerr.Error
		if errors.As(bad.Reason, &se) && !se.Recoverable {
			return 2
		}
	}
	if scanErr != nil {
		var se *mediaerr.Error
		if errors.As(scanErr, &se) && se.Kind == mediaerr.KindTruncated {
			fmt.Println("truncated_tail: complete prefix is recoverable")
			return 0
		}
		return pipelineError(scanErr)
	}
	return 0
}
