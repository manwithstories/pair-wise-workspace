package media

import "fmt"

// Severity classifies a media error.
type Severity int

const (
	// SeverityInfo is informational only.
	SeverityInfo Severity = iota
	// SeverityRecoverable marks damage that was repaired or skipped;
	// processing continues and exit code stays 0.
	SeverityRecoverable
	// SeverityFatal marks unrecoverable corruption; the run fails.
	SeverityFatal
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityRecoverable:
		return "recoverable"
	case SeverityFatal:
		return "fatal"
	}
	return "unknown"
}

// Error is a media error anchored to a track and a time interval.
type Error struct {
	TrackID  string
	Start    Rational // inclusive interval start
	End      Rational // inclusive interval end
	Severity Severity
	Code     string // machine-readable code, e.g. "truncated-mdat"
	Message  string
}

func (e Error) String() string {
	return fmt.Sprintf("[%s] track=%s %s..%s %s: %s",
		e.Severity, e.TrackID, e.Start, e.End, e.Code, e.Message)
}

// List collects errors; the zero value is ready to use.
type List struct {
	errs []Error
}

// Add appends an error.
func (l *List) Add(e Error) { l.errs = append(l.errs, e) }

// Addf builds and appends an error with a formatted message.
func (l *List) Addf(sev Severity, track, code string, start, end Rational, format string, args ...any) {
	l.errs = append(l.errs, Error{
		TrackID:  track,
		Start:    start,
		End:      end,
		Severity: sev,
		Code:     code,
		Message:  fmt.Sprintf(format, args...),
	})
}

// All returns the collected errors.
func (l *List) All() []Error { return l.errs }

// Len returns the number of errors.
func (l *List) Len() int { return len(l.errs) }

// HasFatal reports whether any error is fatal.
func (l *List) HasFatal() bool {
	for _, e := range l.errs {
		if e.Severity == SeverityFatal {
			return true
		}
	}
	return false
}

// Count returns the number of errors at the given severity.
func (l *List) Count(sev Severity) int {
	n := 0
	for _, e := range l.errs {
		if e.Severity == sev {
			n++
		}
	}
	return n
}
