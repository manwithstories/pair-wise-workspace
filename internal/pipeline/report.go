package pipeline

import (
	"fmt"
	"strings"

	"relaycheck/internal/media"
	"relaycheck/internal/verify"
)

// FormatResult renders the stable acceptance report. It emits exactly one
// 64-hex SHA-256 token per segment, followed by the summary metrics.
func FormatResult(r *Result) string {
	var b strings.Builder
	rep := r.Report

	for i, h := range rep.Hashes {
		kf := i
		if i < len(r.Plan.Segments) {
			kf = r.Plan.Segments[i].KeyframeID
		}
		fmt.Fprintf(&b, "segment=%06d keyframe=%d sha256=%s\n", i, kf, h)
	}

	if r.Cfg.Resume {
		fmt.Fprintf(&b, "wal_replay=%d\n", r.WALReplay)
	}
	fmt.Fprintf(&b, "roundtrip_consistency=%s\n", verify.Percent(rep.RoundtripMatched, rep.RoundtripTotal))
	fmt.Fprintf(&b, "keyframe_hit_rate=%s（%d/%d）\n",
		verify.Percent(rep.KeyHits, rep.KeyTotal), rep.KeyHits, rep.KeyTotal)
	if r.Cfg.Resume {
		fmt.Fprintf(&b, "resume_hash_match=%s\n", verify.Percent(r.ResumeMatched, r.ResumeTotal))
	}
	fmt.Fprintf(&b, "segment_hash_match=%s（%d 片段）\n",
		verify.Percent(rep.HashMatched, rep.HashTotal), rep.HashMatched)
	return b.String()
}

// FormatDiagnostics renders non-contract diagnostics for stderr.
func FormatDiagnostics(r *Result) string {
	st := r.Plan.RTP.Stats
	return fmt.Sprintf(
		"meta_conflicts=%d rtp_received=%d duplicates=%d reordered=%d late=%d emitted=%d gaps=%d wraps=%d bandwidth_steps=%d transitions=%d\n",
		r.Plan.MetaConflicts, st.Received, st.Duplicates, st.Reordered, st.LateDropped,
		st.Emitted, st.Gaps, st.Wraps, len(r.Cfg.BandwidthKbps), len(r.Plan.Pacer.Transitions()),
	)
}

// FormatFault renders the required parse_error/track_id/time_range report.
func FormatFault(f *media.Fault) string {
	if f == nil {
		return ""
	}
	code := string(f.Code)
	if code == "" {
		code = string(media.CodeInvalidInput)
	}
	track := int64(0)
	if f.HaveTrack {
		track = f.TrackID
	}
	timeRange := "unavailable"
	if f.Range != nil {
		timeRange = f.Range.String()
	}
	var b strings.Builder
	b.WriteString("status=error\n")
	fmt.Fprintf(&b, "error_code=%s\n", code)
	fmt.Fprintf(&b, "%s: %s\n", code, f.Message)
	fmt.Fprintf(&b, "track_id=%d\n", track)
	fmt.Fprintf(&b, "time_range=%s\n", timeRange)
	fmt.Fprintf(&b, "retryable=%t\n", f.Retryable)
	fmt.Fprintf(&b, "exit_code=%d\n", f.ExitCode())
	return b.String()
}
