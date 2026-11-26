// Package model defines the shared track/sample model and fault types
// used across the scan, repair, index, flow and verify subsystems.
package model

import "fmt"

// RepresentationID identifies one camera + bitrate layer of a session.
type RepresentationID struct {
	Camera  string
	Bitrate uint32 // kbps tier label
}

func (id RepresentationID) String() string {
	return fmt.Sprintf("%s/%dkbps", id.Camera, id.Bitrate)
}

// Sample is one media sample located in a source file. All timestamps
// are integers; Clock is on the shared 90 kHz timeline.
type Sample struct {
	Index     uint32 // index in the source track
	Offset    uint64 // absolute file offset of the sample payload
	Size      uint32
	DTS       int64 // repaired, strictly monotonic, media timescale units
	PTS       int64 // presentation time, media timescale units
	Clock     int64 // presentation time on the shared 90 kHz clock
	Keyframe  bool
	Truncated bool // payload incomplete on disk; excluded from index/output
}

// Gap records a byte range declared by the container but missing on disk.
type Gap struct {
	Offset uint64 // exact file offset where data is missing
	Bytes  uint64 // declared but unreadable bytes
	Reason string
}

// FaultCode classifies recoverable container faults.
type FaultCode string

const (
	FaultBoxOverflow   FaultCode = "box_size_overflow"
	FaultTruncatedMdat FaultCode = "truncated_mdat"
	FaultDTSRegression FaultCode = "dts_regression"
	FaultElstRange     FaultCode = "elst_out_of_range"
	FaultNoSync        FaultCode = "missing_keyframes"
)

// Fault is a detected container fault that the pipeline recovered from.
type Fault struct {
	Code   FaultCode
	Offset uint64 // file offset (or sample index << 32) where detected
	Detail string
}

// Track is one repaired representation track on the shared clock.
type Track struct {
	ID        RepresentationID
	Timescale uint32
	Samples   []Sample
	Gaps      []Gap
	Faults    []Fault
}

// ValidSamples returns the samples that may be indexed and delivered
// (payload fully present on disk).
func (t *Track) ValidSamples() []Sample {
	out := make([]Sample, 0, len(t.Samples))
	for _, s := range t.Samples {
		if !s.Truncated {
			out = append(out, s)
		}
	}
	return out
}
