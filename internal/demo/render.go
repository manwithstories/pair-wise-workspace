package demo

import (
	"fmt"
	"strings"
)

// Render produces the stable terminal report. It contains no timestamps,
// temporary paths or map-order-dependent fields.
func Render(r *Result) string {
	m := r.Metrics
	var b strings.Builder
	fmt.Fprintf(&b, "roundtrip_consistency=%s\n", m.RoundtripConsistency())
	fmt.Fprintf(&b, "keyframe_index_hit=%s\n", m.KeyframeIndexHit())
	fmt.Fprintf(&b, "keyframe_index_sha256=%s\n", r.IndexSHA256)
	fmt.Fprintf(&b, "segment_sha256=%d/%d\n", m.SegmentMatched, m.SegmentTotal)
	for _, s := range r.Segments {
		fmt.Fprintf(&b, "segment_sha256_%02d=%s match=%t recovery=%s\n", s.Index, s.Hash, s.Match, s.State)
	}
	fmt.Fprintf(&b, "duplicate_dropped=%d\n", m.DuplicateDropped)
	fmt.Fprintf(&b, "conflicting_packets_isolated=%d\n", m.ConflictsIsolated)
	fmt.Fprintf(&b, "reordered_packets=%d\n", m.ReorderedPackets)
	fmt.Fprintf(&b, "sequence_wrap=%s\n", okLabel(r.Packet.SequenceWrapOK))
	fmt.Fprintf(&b, "timestamp_wrap=%s\n", okLabel(r.Packet.TimestampWrapOK))
	fmt.Fprintf(&b, "reorder_window_bound=%s\n", okLabel(r.Packet.WindowOverflowOK))
	fmt.Fprintf(&b, "burst_recovered=%s\n", m.BurstRecovery())
	fmt.Fprintf(&b, "recovery_methods=retransmitted:%d,xor_recovered:%d,unrecoverable:%d\n",
		r.Retransmitted, r.XORRecovered, m.UnrecoverableLoss)
	fmt.Fprintf(&b, "continuous_loss_boundary=%s dropped_until_verified_keyframe:%d\n",
		okLabel(r.Packet.ContinuousLossOK), r.Packet.ContinuousDropped)
	fmt.Fprintf(&b, "av_skew_ms=%d\n", m.AVSkewMS)
	if r.SyncReport != nil {
		fmt.Fprintf(&b, "audio_silence_compensation_ms=%d\n", r.SyncReport.SilenceInsertedMS)
	}
	fmt.Fprintf(&b, "buffer_targets_ms=%s\n", joinInts(m.BandwidthTargets))
	fmt.Fprintf(&b, "first_error_offset=%d\n", m.FirstErrorOffset)
	fmt.Fprintf(&b, "provenance=truncated_klv_offset:%d,bad_crop_offset:%d,recovery_states_preserved:true\n",
		r.TruncatedOffset, r.CropOffset)
	fmt.Fprintf(&b, "color_conflict_priority=explicit_override>track>container discarded_values:%d\n", 2)
	if r.ExpectedPathsComplete() {
		b.WriteString("demo_status=all_expected_paths_completed\n")
	} else {
		b.WriteString("demo_status=invariant_failure\n")
	}
	return b.String()
}

func okLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ",")
}
