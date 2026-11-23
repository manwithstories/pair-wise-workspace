// Package fault defines the single error model used across demuxing, index
// reconstruction, checkpointing and output. Every fault carries the first
// confirmable abnormal byte offset, the affected track and whether processing
// can recover by skipping the fragment.
package fault

import "fmt"

// Kind classifies the structural problem that was detected.
type Kind string

const (
	KindTruncatedHeader Kind = "truncated_header" // box/sample header runs past EOF
	KindCorruptSample   Kind = "corrupt_sample"   // sample bytes fail declared bounds/checks
	KindMissingIndex    Kind = "missing_index"    // required sample table box absent
	KindIndexOutOfRange Kind = "index_out_of_range"
	KindUnknownCodec    Kind = "unknown_codec" // required codec config not supported
	KindLimitExceeded   Kind = "limit_exceeded"
	KindCheckpoint      Kind = "checkpoint"
	KindCancelled       Kind = "cancelled"
)

// Strategy tells callers how the pipeline may react.
type Strategy string

const (
	// SkipFragment means the offending sample/fragment may be dropped while
	// the rest of the track keeps being processed.
	SkipFragment Strategy = "skip_fragment"
	// Abort means the container structure is unusable; processing must stop.
	Abort Strategy = "abort"
	// Rollback means state is discarded back to the last committed boundary.
	Rollback Strategy = "rollback"
)

// Fault is the unified error type. It implements error.
type Fault struct {
	Kind       Kind     `json:"kind"`
	Strategy   Strategy `json:"strategy"`
	Offset     int64    `json:"offset"` // first confirmable abnormal byte; -1 if unknown
	TrackID    uint32   `json:"track_id"`
	TrackIndex int      `json:"track_index"` // 0-based trak index, -1 if global
	Message    string   `json:"message"`
}

// New builds a fault with no known track association.
func New(kind Kind, strategy Strategy, offset int64, msg string) *Fault {
	return &Fault{Kind: kind, Strategy: strategy, Offset: offset, TrackIndex: -1}
}

// WithTrack attaches track identifiers and returns the same fault for chaining.
func (f *Fault) WithTrack(id uint32, index int) *Fault {
	f.TrackID, f.TrackIndex = id, index
	return f
}

func (f *Fault) Error() string {
	loc := "offset=-1"
	if f.Offset >= 0 {
		loc = fmt.Sprintf("offset=%d", f.Offset)
	}
	track := ""
	if f.TrackIndex >= 0 {
		track = fmt.Sprintf(" track=%d(id=%d)", f.TrackIndex, f.TrackID)
	}
	return fmt.Sprintf("fault<%s %s strategy=%s%s>: %s", f.Kind, loc, f.Strategy, track, f.Message)
}

// Recoverable reports whether the fault allows the pipeline to continue.
func (f *Fault) Recoverable() bool { return f.Strategy == SkipFragment }

// As extracts a *Fault from any error, returning nil for ordinary errors.
func As(err error) *Fault {
	for err != nil {
		if f, ok := err.(*Fault); ok {
			return f
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			err = u.Unwrap()
			continue
		}
		return nil
	}
	return nil
}

// First returns the fault with the smallest offset, deterministically breaking
// ties by kind. It is used to report one canonical first-error location even
// when several fragments are corrupt.
func First(faults []*Fault) *Fault {
	var best *Fault
	for _, f := range faults {
		if f == nil {
			continue
		}
		if best == nil || f.Offset < best.Offset ||
			(f.Offset == best.Offset && string(f.Kind) < string(best.Kind)) {
			best = f
		}
	}
	return best
}
