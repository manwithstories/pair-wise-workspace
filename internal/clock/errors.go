package clock

import "fmt"

// Confidence states attached to every normalized observation.
type Confidence uint8

const (
	// Confirmed: monotonic input, no correction beyond ordinary scaling.
	Confirmed Confidence = iota
	// Uncertain: a defined repair was applied (33-bit wrap, in-bound drift).
	Uncertain
	// Rejected: timeline violates the model; packaging must be blocked.
	Rejected
)

func (c Confidence) String() string {
	switch c {
	case Confirmed:
		return "confirmed"
	case Uncertain:
		return "uncertain"
	case Rejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// ErrorKind is the stable machine-readable classification of a defect.
type ErrorKind string

const (
	ErrTrackUnknown        ErrorKind = "track_unknown"
	ErrTimescaleInvalid    ErrorKind = "timescale_invalid"
	ErrTfdtRollback        ErrorKind = "tfdt_rollback"
	ErrDriftExceeded       ErrorKind = "drift_exceeded"
	ErrNonMonotonicSamples ErrorKind = "sample_pts_non_monotonic"
)

// TimelineError is one entry of the shared media-clock error model.
type TimelineError struct {
	TrackID uint32
	Kind    ErrorKind
	// At is the master-clock millisecond position where the defect shows.
	At     int64
	Detail string
}

func (e *TimelineError) Error() string {
	return fmt.Sprintf("clock: track %d %s at %dms: %s", e.TrackID, e.Kind, e.At, e.Detail)
}

// HasKind reports whether err is a TimelineError matching the given kind.
func HasKind(err error, kind ErrorKind) bool {
	if te, ok := err.(*TimelineError); ok {
		return te.Kind == kind
	}
	return false
}
