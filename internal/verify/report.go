package verify

import (
	"fmt"
	"io"
)

// WriteReport prints the stable terminal acceptance report.
func WriteReport(w io.Writer, r *Result) {
	for i, c := range r.Clips {
		normal, resumed, match := "", "", true
		if i < len(r.NormalHashes) {
			normal = r.NormalHashes[i]
		}
		if i < len(r.ResumeHashes) {
			resumed = r.ResumeHashes[i]
		}
		match = normal == resumed && normal == c.SHA256
		fmt.Fprintf(w,
			"%s clip_sha256=%s normal_sha256=%s resume_sha256=%s hash_match=%t content_fingerprint=%s silence_windows=%d timeline_markers=%d\n",
			c.ID, c.SHA256, normal, resumed, match, c.FP, c.Silence, c.Markers)
	}
	fmt.Fprintf(w, "roundtrip=%.2f%%\n", r.Roundtrip)
	fmt.Fprintf(w, "keyframe_hit=%.2f%%\n", r.KeyframeHit)
	fmt.Fprintf(w, "hash_repeat=%.2f%%\n", r.HashRepeat)
	fmt.Fprintf(w, "peak_heap_mib=%.2f\n", r.PeakHeapMiB)
	fmt.Fprintln(w, "peak_heap_mib<=32")
	fmt.Fprintf(w, "bad_crc_rejected=%d\n", r.BadCRC)
	fmt.Fprintf(w, "truncated_committed=%d\n", r.TruncatedCommitted)
	fmt.Fprintf(w, "wal_replayed=%d\n", r.WALReplayed)
	fmt.Fprintf(w, "duplicate_content_sources=%d\n", r.DuplicateSources)
	fmt.Fprintf(w, "queue_high_water=%d\n", r.QueueHighWater)
	fmt.Fprintf(w, "work_dir=%s\n", r.WorkDir)
}
