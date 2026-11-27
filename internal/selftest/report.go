package selftest

import (
	"fmt"
	"io"
	"strings"
)

// Print renders the report as a fixed-column terminal table plus a block of
// named boolean checks. Column names match the acceptance vocabulary exactly.
func Print(w io.Writer, rep *Report, pass bool) {
	header := []string{
		"case", "target_bps", "actual_bps", "bitrate_delta_pct",
		"psnr_mean_db", "ssim_mean", "max_alignment_ms",
		"bad_records", "reused_segments", "manifest_sha256_equal",
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, r := range rep.Rows {
		fmt.Fprintf(w, "%s\t%d\t%d\t%.3f\t%.3f\t%.4f\t%.3f\t%d\t%d\t%t\n",
			r.Case, r.TargetBPS, r.ActualBPS, r.BitrateDeltaPct,
			r.PSNRMeanDB, r.SSIMMean, r.MaxAlignmentMS,
			r.BadRecords, r.ReusedSegments, r.ManifestSHAEqual)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "checks:")
	for _, c := range rep.Checks {
		status := "PASS"
		if !c.OK {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %-38s %s\n", status, c.Name, c.Detail)
	}
	fmt.Fprintln(w)
	if pass {
		fmt.Fprintln(w, "RESULT: PASS")
	} else {
		fmt.Fprintln(w, "RESULT: FAIL")
	}
}
