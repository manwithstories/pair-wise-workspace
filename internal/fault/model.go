// Package fault defines the shared recoverable / skippable / fatal error
// model used by the demuxer, segmenter and manifest pipeline. Every anomaly
// is recorded with its file offset and canonical media time so that skipped
// data never silently disappears from the report.
package fault

import "fmt"

// Severity classifies how the pipeline may react to an anomaly.
type Severity int

const (
	// Recoverable anomalies are recorded but the affected data is still
	// usable (e.g. a missing optional index that was rebuilt by scanning).
	Recoverable Severity = iota
	// Skip marks data that cannot be used. Under PolicySkip the affected
	// samples are dropped and the gap is reported; under PolicyStrict any
	// Skip fault aborts processing as a media error.
	Skip
	// Fatal faults always abort processing regardless of policy.
	Fatal
)

func (s Severity) String() string {
	switch s {
	case Recoverable:
		return "recoverable"
	case Skip:
		return "skip"
	case Fatal:
		return "fatal"
	}
	return "unknown"
}

// Policy decides whether skippable faults abort the run.
type Policy int

const (
	// PolicyStrict treats any Skip fault as a media error (exit code 3).
	PolicyStrict Policy = iota
	// PolicySkip drops unusable samples and reports the gaps.
	PolicySkip
)

func (p Policy) String() string {
	if p == PolicySkip {
		return "skip"
	}
	return "strict"
}

// Fault is one recorded anomaly. Offset is the file byte offset (-1 when
// unknown) and MediaTicks is the canonical timeline position (-1 when
// unknown), so a gap can always be located in both domains.
type Fault struct {
	Severity   Severity
	Offset     int64
	TrackID    uint32
	MediaTicks int64
	Message    string
}

func (f Fault) String() string {
	return fmt.Sprintf("[%s] track=%d offset=%d media_ticks=%d: %s",
		f.Severity, f.TrackID, f.Offset, f.MediaTicks, f.Message)
}

// List accumulates faults during processing.
type List struct {
	Faults []Fault
}

// Add records a fault. Offset and ticks may be -1 when not applicable.
func (l *List) Add(sev Severity, offset int64, trackID uint32, ticks int64, format string, args ...any) {
	l.Faults = append(l.Faults, Fault{
		Severity:   sev,
		Offset:     offset,
		TrackID:    trackID,
		MediaTicks: ticks,
		Message:    fmt.Sprintf(format, args...),
	})
}

// MaxSeverity returns the highest severity recorded, or Recoverable when
// the list is empty.
func (l *List) MaxSeverity() Severity {
	max := Recoverable
	for _, f := range l.Faults {
		if f.Severity > max {
			max = f.Severity
		}
	}
	return max
}

// Blocking reports whether the recorded faults must abort the run under
// the given policy.
func (l *List) Blocking(p Policy) bool {
	max := l.MaxSeverity()
	if max >= Fatal {
		return true
	}
	return p == PolicyStrict && max >= Skip
}

// CountBySeverity tallies faults per severity for reporting.
func (l *List) CountBySeverity() map[Severity]int {
	out := map[Severity]int{}
	for _, f := range l.Faults {
		out[f.Severity]++
	}
	return out
}
