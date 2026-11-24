package core

import (
	"errors"
	"fmt"
	"strings"
)

// FaultKind enumerates the defect classes understood across subsystems. The
// same vocabulary is emitted by manifest parsing, RTP recovery, MP4
// inspection and scheduling so reports keep one timeline.
type FaultKind string

const (
	FaultPathEscape        FaultKind = "path_escape"         // reference escapes input root
	FaultMissingInit       FaultKind = "missing_init"        // initialization segment absent
	FaultTimestampBackward FaultKind = "timestamp_backward"  // illegal non-monotonic timestamp
	FaultDuplicatePacket   FaultKind = "rtp_duplicate"       // repeated RTP sequence number
	FaultReorderedPacket   FaultKind = "rtp_reordered"       // out-of-order packet inside window
	FaultBurstLoss         FaultKind = "rtp_burst_loss"      // unrecovered sequence gap
	FaultLatePacket        FaultKind = "rtp_late"            // packet arriving past the window
	FaultTruncatedBox      FaultKind = "truncated_box"       // moof/mdad shorter than declared
	FaultZeroDuration      FaultKind = "zero_duration_sample"
	FaultInvalidNAL        FaultKind = "invalid_nal"         // unparseable AVC/HEVC unit
	FaultBlocked           FaultKind = "blocked_dependency"  // DAG node blocked upstream
	FaultRetryExhausted    FaultKind = "retry_exhausted"
)

// Severity ranks how strongly a fault affects downstream artifacts.
type Severity string

const (
	SevWarn     Severity = "warn"     // recovered, no data loss
	SevDamaged  Severity = "damaged"  // segment range must not be remuxed
	SevBlocking Severity = "blocking" // node cannot run at all
)

// Source locates a fault inside the delivery tree. Line is 1-based and only
// set for manifest text; Packet/Sample identify transport or bitstream units.
type Source struct {
	RepID         string `json:"rep_id,omitempty"`
	Segment       string `json:"segment,omitempty"`
	SegmentIndex  int    `json:"segment_index,omitempty"`
	Line           int    `json:"line,omitempty"`
	PacketSeq      uint16 `json:"packet_seq,omitempty"`
	SampleIndex    int    `json:"sample_index,omitempty"`
	OffsetBytes    int64  `json:"offset_bytes,omitempty"`
}

// TimeRange marks the affected span on the unified millisecond clock.
type TimeRange struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
}

// Fault is the single error record shared by all modules.
type Fault struct {
	Kind     FaultKind `json:"kind"`
	Severity Severity  `json:"severity"`
	Message  string    `json:"message"`
	Source   Source    `json:"source"`
	Range    *TimeRange `json:"range,omitempty"`
}

func (f Fault) Error() string {
	var b strings.Builder
	b.WriteString(string(f.Kind))
	if f.Source.RepID != "" {
		fmt.Fprintf(&b, " rep=%s", f.Source.RepID)
	}
	if f.Source.Segment != "" {
		fmt.Fprintf(&b, " segment=%s", f.Source.Segment)
	}
	if f.Source.Line > 0 {
		fmt.Fprintf(&b, " line=%d", f.Source.Line)
	}
	b.WriteString(": ")
	b.WriteString(f.Message)
	return b.String()
}

// FaultError wraps a single fault so it satisfies the standard error interface.
type FaultError struct{ F Fault }

func (e *FaultError) Error() string { return e.F.Error() }

// AsFault extracts a Fault from any error produced by the tool.
func AsFault(err error) (Fault, bool) {
	var fe *FaultError
	if errors.As(err, &fe) {
		return fe.F, true
	}
	return Fault{}, false
}

// Recorder collects faults in a deterministic order. It is safe for
// concurrent use because DAG workers may inspect different segments at once.
type Recorder struct {
	mu     chan struct{}
	faults []Fault
}

// NewRecorder creates an empty recorder.
func NewRecorder() *Recorder {
	r := &Recorder{mu: make(chan struct{}, 1)}
	r.mu <- struct{}{}
	return r
}

// Add appends a fault.
func (r *Recorder) Add(f Fault) {
	<-r.mu
	r.faults = append(r.faults, f)
	r.mu <- struct{}{}
}

// AddErr appends a fault carried by an error; non-fault errors are coerced
// into a blocking fault with the error message.
func (r *Recorder) AddErr(err error) {
	if err == nil {
		return
	}
	if f, ok := AsFault(err); ok {
		r.Add(f)
		return
	}
	r.Add(Fault{Kind: "internal", Severity: SevBlocking, Message: err.Error()})
}

// All returns a copy of the recorded faults.
func (r *Recorder) All() []Fault {
	<-r.mu
	out := make([]Fault, len(r.faults))
	copy(out, r.faults)
	r.mu <- struct{}{}
	return out
}

// HasDamaged reports whether any fault prevents remux of a range.
func (r *Recorder) HasDamaged() bool {
	for _, f := range r.All() {
		if f.Severity == SevDamaged || f.Severity == SevBlocking {
			return true
		}
	}
	return false
}

// NewFault is a compact constructor for module authors.
func NewFault(kind FaultKind, sev Severity, msg string, src Source) Fault {
	return Fault{Kind: kind, Severity: sev, Message: msg, Source: src}
}
