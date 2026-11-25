package demo

import (
	"fmt"
	"strings"
)

// Lines returns the stable terminal acceptance report.
func (r *Report) Lines() []string {
	var b strings.Builder
	fmt.Fprintf(&b, "fieldpost demo seed=%d profile=%s\n", r.Seed, r.Profile)
	fmt.Fprintf(&b, "output_dir=%s\n", r.OutputDir)
	fmt.Fprintf(&b, "healthy.roundtrip=%.3f\n", r.HealthyRoundtrip)
	fmt.Fprintf(&b, "recovery.keyframe_hit=%.3f\n", r.KeyframeHit)
	fmt.Fprintf(&b, "essence_sha256=%d/%d\n", r.EssenceOK, r.EssenceTotal)
	fmt.Fprintf(&b, "peak_mem_bytes=%d\n", r.PeakMemBytes)
	fmt.Fprintf(&b, "memory_cap_bytes=%d\n", r.MemoryCap)
	fmt.Fprintf(&b, "backpressure_events=%d\n", r.BackpressureEvents)
	fmt.Fprintf(&b, "boundary.rejected=%d\n", r.BoundaryRejected)
	fmt.Fprintf(&b, "unrecoverable.isolated=%d\n", r.Unrecoverable)
	fmt.Fprintf(&b, "unrecoverable.outputs=%d\n", r.UnrecoverableOutputs)
	fmt.Fprintf(&b, "determinism.stable=%t\n", r.Deterministic)
	fmt.Fprintf(&b, "bandwidth.profiles=%d\n", r.BandwidthProfiles)
	for _, h := range r.Healthy {
		fmt.Fprintf(&b, "segment %s format=%s sha256=%s keyframes=%d/%d rtp_retries=%d dup=%d reorder=%d\n",
			h.ID, h.Format, h.Hash[:16], h.KeyHits, h.KeyTotal, h.Retries, h.Duplicates, h.Reordered)
	}
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

// Output returns the complete report text.
func (r *Report) Output() string {
	return strings.Join(r.Lines(), "\n") + "\n"
}
