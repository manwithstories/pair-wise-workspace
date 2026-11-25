package report

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Render 以稳定的 key=value 形式向 w 打印验收要求的全部字段。
// 字段名固定：segments、samples、first_bad、offset_delta_bytes、
// keyframe_gap_ms、memory_peak_bytes、resume_ms、sha256。
func Render(w io.Writer, s Summary) {
	fmt.Fprintf(w, "segments=%d\n", s.Segments)
	fmt.Fprintf(w, "samples=%d\n", s.Samples)
	fmt.Fprintf(w, "first_bad=%d\n", s.FirstBad)
	fmt.Fprintf(w, "offset_delta_bytes=%d\n", s.OffsetDeltaBytes)
	fmt.Fprintf(w, "keyframe_gap_ms=%s\n", joinInt64(s.KeyframeGapMS))
	fmt.Fprintf(w, "memory_peak_bytes=%d\n", s.MemoryPeakBytes)
	fmt.Fprintf(w, "resume_ms=%d\n", s.ResumeMS)
	fmt.Fprintf(w, "sha256=%s\n", s.SHA256)
	fmt.Fprintf(w, "fault_count=%d\n", s.FaultCount)
	for _, f := range s.Faults {
		fmt.Fprintf(w, "fault: %s\n", f.String())
	}
}

func joinInt64(xs []int64) string {
	if len(xs) == 0 {
		return ""
	}
	parts := make([]string, len(xs))
	for i, v := range xs {
		parts[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(parts, ",")
}
