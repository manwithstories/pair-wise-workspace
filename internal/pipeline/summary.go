package pipeline

import (
	"fmt"
	"strings"
)

// Summary renders the human-readable terminal summary. Every acceptance field
// appears explicitly on its own line or as a labelled table.
func (r *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "mediasync report v%d (seed=%d)\n", r.Version, r.Seed)
	fmt.Fprintf(&b, "roundtrip_consistency : %.4f\n", r.RoundtripConsistency)
	fmt.Fprintf(&b, "keyframe_hit_rate     : %.4f\n", r.KeyframeHitRate)
	fmt.Fprintf(&b, "segment_count         : %d\n", r.SegmentCount)
	fmt.Fprintf(&b, "segment_sha256        : %s\n", strings.Join(r.SegmentSHA256, ","))
	fmt.Fprintf(&b, "duplicate_packets     : %d\n", r.DuplicatePackets)
	fmt.Fprintf(&b, "reordered_packets     : %d\n", r.ReorderedPackets)
	fmt.Fprintf(&b, "burst_lost_packets    : %d\n", r.BurstLostPackets)
	fmt.Fprintf(&b, "errors_with_offset    : %d\n", r.ErrorsWithOffset)
	if len(r.BandwidthKbps) > 0 {
		fmt.Fprintf(&b, "bandwidth_profile     : %s (windows=%d shaped_drops=%d)\n",
			joinInts(r.BandwidthKbps), r.BandwidthWindows, r.ShapedDrops)
	}
	fmt.Fprintf(&b, "issues total=%d recoverable=%d expected=%d fatal=%v\n",
		r.TotalIssues, r.RecoverableIssues, r.ExpectedIssues, r.fatal)
	b.WriteString("stages:\n")
	for _, s := range r.Stages {
		status := "ok"
		if !s.OK {
			status = "FAILED"
		}
		fmt.Fprintf(&b, "  %-12s %-6s issues=%d %s\n", s.Name, status, s.Issues, s.Detail)
	}
	b.WriteString("segments:\n")
	for _, s := range r.Segments {
		deg := ""
		if len(s.Degrade) > 0 {
			deg = " degrade=" + strings.Join(s.Degrade, "+")
		}
		fmt.Fprintf(&b, "  %s#%02d %8.3fs..%8.3fs hash64=%s%s\n",
			s.Camera, s.Index, float64(s.StartMS)/1000, float64(s.EndMS)/1000, s.Hash64, deg)
	}
	if len(r.Matches) > 0 {
		b.WriteString("cross-camera duplicates:\n")
		for _, m := range r.Matches {
			fmt.Fprintf(&b, "  %s cameras=%s count=%d\n", m.Hash64, strings.Join(m.Cameras, ","), m.Count)
		}
	}
	if r.fatal {
		b.WriteString("result: FATAL\n")
	} else {
		b.WriteString("result: OK\n")
	}
	return b.String()
}
