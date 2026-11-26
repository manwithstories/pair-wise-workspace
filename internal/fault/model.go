// Package fault defines the shared error model used by every stage of the
// media kernel: each fault is located by track, frame sequence number and a
// half-open byte range, and carries a recoverability verdict.
package fault

import (
	"fmt"
	"sort"
	"sync"
)

// Kind classifies a detected fault.
type Kind string

const (
	KindHashMismatch      Kind = "hash_mismatch"
	KindTruncated         Kind = "truncated"
	KindBadTimestamp      Kind = "bad_timestamp"
	KindStructure         Kind = "structure"
	KindUnsupportedLayout Kind = "unsupported_layout"
	KindUnknownColor      Kind = "unknown_color"
	KindMissingFrame      Kind = "missing_frame"
	KindKeyframeInterval  Kind = "keyframe_interval"
)

// ByteRange is a half-open byte range [Start, End) inside a track file.
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Fault locates a defect on a track. Frame is the frame sequence number,
// or -1 when the fault is not scoped to a single frame.
type Fault struct {
	Track       string    `json:"track"`
	Frame       int64     `json:"frame"`
	Bytes       ByteRange `json:"bytes"`
	Kind        Kind      `json:"kind"`
	Recoverable bool      `json:"recoverable"`
	Detail      string    `json:"detail"`
}

func (f Fault) String() string {
	return fmt.Sprintf("%s track=%s frame=%d bytes=[%d,%d) recoverable=%t: %s",
		f.Kind, f.Track, f.Frame, f.Bytes.Start, f.Bytes.End, f.Recoverable, f.Detail)
}

// List is a concurrency-safe collection of faults.
type List struct {
	mu    sync.Mutex
	items []Fault
}

// Add appends a fault.
func (l *List) Add(f Fault) {
	l.mu.Lock()
	l.items = append(l.items, f)
	l.mu.Unlock()
}

// Items returns a sorted copy: by track, then frame, then kind.
func (l *List) Items() []Fault {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Fault, len(l.items))
	copy(out, l.items)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		if a.Frame != b.Frame {
			return a.Frame < b.Frame
		}
		return a.Kind < b.Kind
	})
	return out
}

// Len reports the number of recorded faults.
func (l *List) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}

// HasUnrecoverable reports whether any fault is marked unrecoverable.
func (l *List) HasUnrecoverable() bool {
	for _, f := range l.Items() {
		if !f.Recoverable {
			return true
		}
	}
	return false
}
