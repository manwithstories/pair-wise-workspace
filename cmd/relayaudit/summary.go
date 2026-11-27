package main

import (
	"fmt"
	"time"

	"relayaudit/internal/fixture"
	"relayaudit/internal/pipeline"
	"relayaudit/internal/verify"
)

func printSummary(o *options, seg *fixture.Segment, out *pipeline.Outcome, r *verify.Result, elapsed time.Duration) {
	fmt.Printf("seed=%d size=%d source_bytes=%d\n", o.seed, o.size, len(seg.Bytes))
	fmt.Printf("tracks=%d records_declared=%d kept=%d dropped=%d\n",
		seg.TrackCount, recordsDeclared(out), out.Kept, out.Dropped)
	fmt.Printf("damage_ranges=%d\n", r.DamageRanges)
	for _, rg := range r.Ranges {
		fmt.Printf("  range kind=%s offset=%d end=%d bytes=%d clips=%v\n",
			rg.Kind, rg.Start, rg.End, rg.Length(), rg.Clips)
	}
	fmt.Printf("corrupt_offset_delta=%d\n", r.CorruptOffsetDelta)
	fmt.Printf("truncated_tail_bytes=%d\n", r.TruncatedTailBytes)
	fmt.Printf("index_rebuilt=%d\n", r.IndexRebuilt)
	fmt.Printf("resume_ms=%d\n", r.ResumeMillis)
	fmt.Printf("output_sha256=%s\n", r.OutputSHA256)
	fmt.Printf("repeat_sha256_equal=%d\n", r.RepeatSHA256Equal)
	fmt.Printf("frames=%d key_interval_ticks=%v proxy_hash=%s\n", r.Frames, r.KeyIntervalTicks, r.ProxyHash)
	fmt.Printf("elapsed_ms=%d\n", elapsed.Milliseconds())
}

// recordsDeclared returns the source record count as parsed by the pipeline.
func recordsDeclared(out *pipeline.Outcome) int {
	if out == nil || out.Scan == nil {
		return 0
	}
	return int(out.Scan.Header.RecordCount)
}
