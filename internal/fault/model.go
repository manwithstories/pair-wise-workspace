// Package fault defines the cross-module error model shared by every stage
// of the media-delivery pipeline. Every anomaly (missing or corrupt segment,
// out-of-order sequence, metadata conflict, ...) is represented as a Fault so
// that the report and the recovery logic use the same vocabulary.
package fault

import (
	"fmt"
	"sync"
)

// Kind enumerates the anomaly categories the pipeline can detect.
type Kind string

const (
	// KindPathTraversal: a manifest URI escapes the manifest directory.
	KindPathTraversal Kind = "path_traversal"
	// KindDuplicateSeq: the same segment sequence number appears twice.
	KindDuplicateSeq Kind = "duplicate_seq"
	// KindOutOfOrder: segment sequence numbers are not monotonically increasing.
	KindOutOfOrder Kind = "out_of_order"
	// KindMissingSegment: a segment referenced by a manifest is absent.
	KindMissingSegment Kind = "missing_segment"
	// KindTruncatedPacket: a packet/segment ended before its declared framing.
	KindTruncatedPacket Kind = "truncated_packet"
	// KindPTSReset: a timestamp reset or wraparound was observed and normalised.
	KindPTSReset Kind = "pts_reset"
	// KindSpecConflict: stream specifications (colour/sample rate/layout) clash.
	KindSpecConflict Kind = "spec_conflict"
	// KindDecode: a packet could not be decoded.
	KindDecode Kind = "decode_error"
	// KindIO: a local filesystem write/read failed.
	KindIO Kind = "io_error"
	// KindCheckpoint: a checkpoint was missing, corrupt or stale.
	KindCheckpoint Kind = "checkpoint_error"
)

// Fault is a single, structured anomaly.
type Fault struct {
	// Kind is the anomaly category.
	Kind Kind `json:"kind"`
	// Camera identifies the source camera ("stage", "side", "audience", ...).
	Camera string `json:"camera"`
	// Track identifies the media track ("video", "audio", ...).
	Track string `json:"track"`
	// Sequence is the segment sequence number.
	Sequence int `json:"sequence"`
	// HasSequence reports whether Sequence is meaningful (sequence 0 is valid).
	HasSequence bool `json:"has_sequence"`
	// Offset90k is the first anomalous position on the 90 kHz media clock.
	Offset90k int64 `json:"offset_90k"`
	// HasOffset reports whether Offset90k is meaningful (offset 0 is valid).
	HasOffset bool `json:"has_offset"`
	// Recoverable reports whether a fallback representation or another
	// remediation can satisfy this slot.
	Recoverable bool `json:"recoverable"`
	// Recovered is set true once a fallback actually filled the slot.
	Recovered bool `json:"recovered"`
	// Detail is a human readable explanation.
	Detail string `json:"detail"`
}

// New builds a Fault with the sentinel fields initialised. seq and offset are
// positional values paired with their presence flags so a legitimate zero is
// not confused with "not applicable".
func New(kind Kind, camera, track string, hasSeq bool, seq int, hasOffset bool, offset int64, recoverable bool, detail string) Fault {
	f := Fault{
		Kind:        kind,
		Camera:      camera,
		Track:       track,
		HasSequence: hasSeq,
		Sequence:    seq,
		HasOffset:   hasOffset,
		Offset90k:   offset,
		Recoverable: recoverable,
		Detail:      detail,
	}
	if !hasSeq {
		f.Sequence = -1
	}
	if !hasOffset {
		f.Offset90k = -1
	}
	return f
}

// Error makes Fault satisfy the error interface.
func (f Fault) Error() string {
	return fmt.Sprintf("fault[%s] camera=%s track=%s seq=%d offset90k=%d recoverable=%t recovered=%t: %s",
		f.Kind, f.Camera, f.Track, f.Sequence, f.Offset90k, f.Recoverable, f.Recovered, f.Detail)
}

// Recorder collects faults in deterministic encounter order and keeps simple
// counters so the report layer does not have to re-scan the slice. It is safe
// for concurrent use because pipeline workers record faults in parallel.
type Recorder struct {
	mu     sync.Mutex
	faults []Fault
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Record appends a fault. It returns the recorded fault for convenience.
func (r *Recorder) Record(f Fault) Fault {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.faults = append(r.faults, f)
	return f
}

// Add builds and records a fault that carries both a sequence and an offset.
func (r *Recorder) Add(kind Kind, camera, track string, seq int, offset int64, recoverable bool, detail string) Fault {
	return r.Record(New(kind, camera, track, true, seq, true, offset, recoverable, detail))
}

// AddNoSeq records a manifest-level fault without segment positioning.
func (r *Recorder) AddNoSeq(kind Kind, camera, track string, recoverable bool, detail string) Fault {
	return r.Record(New(kind, camera, track, false, 0, false, 0, recoverable, detail))
}

// Faults returns a copy of the recorded faults.
func (r *Recorder) Faults() []Fault {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Fault, len(r.faults))
	copy(out, r.faults)
	return out
}

// Len returns the number of recorded faults.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.faults)
}

// CountByKind groups faults by Kind.
func (r *Recorder) CountByKind() map[Kind]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[Kind]int)
	for _, f := range r.faults {
		m[f.Kind]++
	}
	return m
}

// CountRecoverable / CountRecovered drive the recovery-rate metric.
func (r *Recorder) CountRecoverable() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, f := range r.faults {
		if f.Recoverable {
			n++
		}
	}
	return n
}

// CountRecovered counts faults that were actually remediated.
func (r *Recorder) CountRecovered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, f := range r.faults {
		if f.Recovered {
			n++
		}
	}
	return n
}

// MarkRecovered flags every fault matching kind+sequence as recovered. It
// returns the number of faults updated.
func (r *Recorder) MarkRecovered(kind Kind, sequence int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for i := range r.faults {
		if r.faults[i].Kind == kind && r.faults[i].Sequence == sequence {
			r.faults[i].Recovered = true
			n++
		}
	}
	return n
}

// HasUnrecoverable reports whether any fault could not be remediated.
func (r *Recorder) HasUnrecoverable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.faults {
		if !f.Recoverable {
			return true
		}
	}
	return false
}

// Unrecoverable returns the subset of faults that cannot be remediated.
func (r *Recorder) Unrecoverable() []Fault {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Fault
	for _, f := range r.faults {
		if !f.Recoverable {
			out = append(out, f)
		}
	}
	return out
}
