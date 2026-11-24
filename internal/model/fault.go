package model

import "fmt"

// FaultCode identifies the class of a located defect. Codes are stable
// strings so manifests and checkpoints can compare across runs.
type FaultCode string

const (
	FaultSeqDuplicate        FaultCode = "seq_duplicate"
	FaultSeqGap              FaultCode = "seq_gap"
	FaultHashMismatch        FaultCode = "hash_mismatch"
	FaultTimestampRegression FaultCode = "timestamp_regression"
	FaultBoxTruncated        FaultCode = "box_truncated"
	FaultSampleOffset        FaultCode = "sample_offset_out_of_range"
	FaultNoIndex             FaultCode = "no_index"
	FaultAudioConflict       FaultCode = "audio_conflict"
	FaultAudioIsolated       FaultCode = "audio_isolated"
	FaultColorMissing        FaultCode = "color_missing"
	FaultColorConflict       FaultCode = "color_conflict"
	FaultUnknownAdapter      FaultCode = "unknown_adapter"
	FaultCheckpointInvalid   FaultCode = "checkpoint_invalid"
)

// Fault is a structured, located defect. Segment is -1 when not yet
// attributable; Range is on the unified clock when known.
type Fault struct {
	Code    FaultCode
	Adapter string // originating adapter id, "" when raised by the core
	Segment int
	Range   TimeRange
	Detail  string
}

func (f Fault) Error() string {
	return fmt.Sprintf("%s seg=%d range=[%d,%d) adapter=%q: %s",
		f.Code, f.Segment, f.Range.Start, f.Range.End, f.Adapter, f.Detail)
}

// Faults is an ordered fault list.
type Faults []Fault

// HasCode reports whether any fault carries the code.
func (fs Faults) HasCode(c FaultCode) bool {
	for _, f := range fs {
		if f.Code == c {
			return true
		}
	}
	return false
}

// Segments returns the sorted set of segment indices touched by faults.
func (fs Faults) Segments() []int {
	seen := map[int]bool{}
	var out []int
	for _, f := range fs {
		if f.Segment >= 0 && !seen[f.Segment] {
			seen[f.Segment] = true
			out = append(out, f.Segment)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
