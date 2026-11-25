package base

import "fmt"

// FaultKind enumerates the fault classes handled by the pipeline.
type FaultKind string

const (
	FaultIndexDrift     FaultKind = "index-drift"
	FaultTruncatedAU    FaultKind = "trunc-au"
	FaultClockGap       FaultKind = "clock-gap"
	FaultRTPLoss        FaultKind = "rtp-loss"
	FaultRTPOrder       FaultKind = "rtp-order"
	FaultBoundary       FaultKind = "boundary"
	FaultColorConflict  FaultKind = "color-conflict"
	FaultOversize       FaultKind = "oversize"
	FaultDuplicateIndex FaultKind = "duplicate-index"
)

// Fault is the structured error type carried through every stage. It always
// retains camera, rendition, PTS and byte offset so a bad fragment can be
// located without reconstructing context from logs.
type Fault struct {
	Kind        FaultKind
	Camera      string
	Rendition    string
	PTS          uint64 // shared 90 kHz clock position
	ByteOffset   int64  // offset inside the source bare stream
	Recoverable  bool
	Detail       string
}

// Error implements error.
func (f *Fault) Error() string {
	rec := "fatal"
	if f.Recoverable {
		rec = "recoverable"
	}
	return fmt.Sprintf("%s fault %s camera=%s rendition=%s pts=%d offset=%d: %s",
		rec, f.Kind, f.Camera, f.Rendition, f.PTS, f.ByteOffset, f.Detail)
}

// Locator is the subset of Fault used when a stage only knows where a
// problem occurred but not yet which class it belongs to.
type Locator struct {
	Camera    string
	Rendition string
	PTS       uint64
	Offset    int64
}

// NewFault builds a fully qualified fault.
func NewFault(kind FaultKind, loc Locator, recoverable bool, format string, args ...any) *Fault {
	return &Fault{
		Kind:        kind,
		Camera:      loc.Camera,
		Rendition:   loc.Rendition,
		PTS:         loc.PTS,
		ByteOffset:  loc.Offset,
		Recoverable: recoverable,
		Detail:      fmt.Sprintf(format, args...),
	}
}

// AsFault extracts a *Fault from an error, returning nil if absent.
func AsFault(err error) *Fault {
	if err == nil {
		return nil
	}
	if f, ok := err.(*Fault); ok {
		return f
	}
	return nil
}
