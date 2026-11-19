package model

import (
	"fmt"
	"sort"
	"strings"
)

// FaultCode is a stable machine-readable fault identifier.
type FaultCode string

// Fault codes are grouped by pipeline stage. The string values are part of the
// JSON contract and must not change.
const (
	CodeInvalidInput      FaultCode = "invalid_input"
	CodeTruncatedBox      FaultCode = "truncated_box"
	CodeCorruptRange      FaultCode = "corrupt_range"
	CodeMissingSample     FaultCode = "missing_sample"
	CodeMissingDuration   FaultCode = "missing_duration"
	CodeNoKeyframe        FaultCode = "no_keyframe"
	CodeKeyframeSnap      FaultCode = "keyframe_snap"
	CodeTimestampWrap     FaultCode = "timestamp_wrap"
	CodeClockDrift        FaultCode = "clock_drift"
	CodeTimestampRegress  FaultCode = "timestamp_regression"
	CodeDuplicatePacket   FaultCode = "duplicate_packet"
	CodeOutOfOrder        FaultCode = "out_of_order_packet"
	CodeLatePacket        FaultCode = "late_packet"
	CodePacketLoss        FaultCode = "packet_loss"
	CodeDependencyCycle   FaultCode = "dependency_cycle"
	CodeMissingParent     FaultCode = "missing_parent"
	CodeAudioUnderCover   FaultCode = "audio_under_cover"
	CodeBlockedDependency FaultCode = "blocked_dependency"
	CodeUnrecoverable     FaultCode = "unrecoverable_range"
	CodeVFRGap            FaultCode = "vfr_missing_duration"
	CodeOverlap           FaultCode = "clip_overlap"
	CodeSourceShort       FaultCode = "source_short"
)

// Severity ranks a fault's impact.
type Severity string

const (
	SevInfo  Severity = "info"
	SevWarn  Severity = "warn"
	SevError Severity = "error"
	SevFatal Severity = "fatal"
)

// Fault is one locatable defect. Every field is optional except Code/Severity.
// Byte ranges and packet sequences are inclusive.
type Fault struct {
	Code       FaultCode `json:"code"`
	Severity   Severity  `json:"severity"`
	Track      string    `json:"track,omitempty"`
	Clip       string    `json:"clip,omitempty"`
	Transition string    `json:"transition,omitempty"`
	ByteStart  int64     `json:"byte_start,omitempty"`
	ByteEnd    int64     `json:"byte_end,omitempty"`
	PacketSeq  uint32    `json:"packet_seq,omitempty"`
	TimeOrig   string    `json:"time_original,omitempty"`
	TimeNorm   string    `json:"time_normalized,omitempty"`
	Value      int64     `json:"value,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Blocking   bool      `json:"blocking,omitempty"`
}

// NewFault builds a fault with the given code and severity.
func NewFault(code FaultCode, sev Severity, format string, args ...any) Fault {
	return Fault{Code: code, Severity: sev, Detail: fmt.Sprintf(format, args...)}
}

// String renders a one-line human readable form used by the terminal report.
func (f Fault) String() string {
	var b strings.Builder
	b.WriteString(string(f.Severity))
	b.WriteString(" ")
	b.WriteString(string(f.Code))
	if f.Track != "" {
		fmt.Fprintf(&b, " track=%s", f.Track)
	}
	if f.Clip != "" {
		fmt.Fprintf(&b, " clip=%s", f.Clip)
	}
	if f.Transition != "" {
		fmt.Fprintf(&b, " transition=%s", f.Transition)
	}
	if f.ByteEnd > 0 || f.ByteStart > 0 {
		fmt.Fprintf(&b, " bytes=[%d,%d]", f.ByteStart, f.ByteEnd)
	}
	if f.PacketSeq > 0 {
		fmt.Fprintf(&b, " seq=%d", f.PacketSeq)
	}
	if f.Detail != "" {
		fmt.Fprintf(&b, " %s", f.Detail)
	}
	return b.String()
}

// FaultList accumulates faults and answers exit-code questions about them.
type FaultList []Fault

// Add appends faults.
func (l *FaultList) Add(fs ...Fault) { *l = append(*l, fs...) }

// Filter returns all faults with the given code.
func (l FaultList) Filter(code FaultCode) []Fault {
	out := make([]Fault, 0, 4)
	for _, f := range l {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

// Has reports whether any fault carries the given code.
func (l FaultList) Has(code FaultCode) bool {
	for _, f := range l {
		if f.Code == code {
			return true
		}
	}
	return false
}

// MaxSeverity returns the highest severity present.
func (l FaultList) MaxSeverity() Severity {
	rank := map[Severity]int{SevInfo: 0, SevWarn: 1, SevError: 2, SevFatal: 3}
	best := SevInfo
	for _, f := range l {
		if rank[f.Severity] > rank[best] {
			best = f.Severity
		}
	}
	return best
}

// Unrecoverable reports whether any fault is fatal and not mitigated.
func (l FaultList) Unrecoverable() bool {
	for _, f := range l {
		if f.Severity == SevFatal {
			return true
		}
	}
	return false
}

// ExitCode maps the fault list to the CLI contract:
// 2 for invalid input, 3 for unrecoverable segments, 0 otherwise.
func (l FaultList) ExitCode() int {
	if l.Has(CodeInvalidInput) {
		return 2
	}
	if l.Unrecoverable() {
		return 3
	}
	return 0
}

// Sort orders faults deterministically: track, clip, byte offset, then code.
func (l FaultList) Sort() {
	sort.SliceStable(l, func(i, j int) bool {
		a, b := l[i], l[j]
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		if a.Clip != b.Clip {
			return a.Clip < b.Clip
		}
		if a.ByteStart != b.ByteStart {
			return a.ByteStart < b.ByteStart
		}
		return a.Code < b.Code
	})
}
