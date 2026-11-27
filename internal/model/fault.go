// Package model holds the media domain types shared by ingest, inspect,
// recover, remux and report.  Everything here is pure data: the packages that
// produce and consume these values carry the behaviour.
package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Track identifies the elementary stream a packet belongs to.
type Track uint8

const (
	TrackAudio Track = iota + 1
	TrackVideo
)

// String renders the track for terminal output.
func (t Track) String() string {
	switch t {
	case TrackAudio:
		return "audio"
	case TrackVideo:
		return "video"
	default:
		return "unknown"
	}
}

// ParseTrack accepts the human names used by command line flags.
func ParseTrack(s string) (Track, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "audio", "a":
		return TrackAudio, nil
	case "video", "v":
		return TrackVideo, nil
	default:
		return 0, fmt.Errorf("model: unknown track %q", s)
	}
}

// FaultKind classifies one problem found while scanning a packet stream.
// The numeric ranges are stable because report output and tests group by
// them.
type FaultKind uint8

const (
	// FaultNone marks a clean observation.
	FaultNone FaultKind = iota

	// Sequence faults.
	FaultDuplicate       // repeated sequence number, counted once
	FaultLoss            // one or more missing sequence numbers
	FaultSequenceWrap    // sequence counter wrapped at the 16 bit boundary
	FaultTimestampRegres // presentation timestamp moved backwards

	// Structural faults.
	FaultTruncatedHeader // frame header shorter than its declared minimum
	FaultBadLength       // declared length exceeds the remaining data
	FaultCorruptPayload  // payload failed the track's structural check

	// Duration faults.
	FaultDurationChange // Opus/AAC packet duration differs from the running mode
	FaultZeroDuration   // a packet declared a zero length span

	// Recovery outcomes, recorded by recover.
	FaultFECRecovered // Opus in-band FEC reconstructed a slot
	FaultPLCReplaced  // libopus PLC substitutions filled a slot
	FaultSilenceFill  // silence placeholder filled a slot
)

// Severity groups faults by how they affect deliverability.
type Severity uint8

const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityError
)

// Severity returns the severity used for exit code and reporting decisions.
func (k FaultKind) Severity() Severity {
	switch k {
	case FaultNone, FaultSequenceWrap, FaultDurationChange:
		return SeverityInfo
	case FaultDuplicate, FaultTimestampRegres, FaultZeroDuration, FaultFECRecovered,
		FaultPLCReplaced, FaultSilenceFill:
		return SeverityWarning
	default:
		return SeverityError
	}
}

// String returns a stable machine-readable fault name.
func (k FaultKind) String() string {
	switch k {
	case FaultNone:
		return "none"
	case FaultDuplicate:
		return "duplicate"
	case FaultLoss:
		return "loss"
	case FaultSequenceWrap:
		return "sequence_wrap"
	case FaultTimestampRegres:
		return "timestamp_regression"
	case FaultTruncatedHeader:
		return "truncated_header"
	case FaultBadLength:
		return "bad_length"
	case FaultCorruptPayload:
		return "corrupt_payload"
	case FaultDurationChange:
		return "duration_change"
	case FaultZeroDuration:
		return "zero_duration"
	case FaultFECRecovered:
		return "fec_recovered"
	case FaultPLCReplaced:
		return "plc_replaced"
	case FaultSilenceFill:
		return "silence_fill"
	default:
		return "unknown"
	}
}

// Slot is a half-open span of the global packet timeline, in nanoseconds.
// Spans always have End > Start; a single packet covers [Start, End).  Slots
// let faults be merged and compared without caring whether they came from
// audio or video.
type Slot struct {
	Start int64
	End   int64
}

// Duration returns the span length.
func (s Slot) Duration() int64 { return s.End - s.Start }

// Overlaps reports whether two slots share at least one nanosecond.
func (s Slot) Overlaps(other Slot) bool { return s.Start < other.End && other.Start < s.End }

// Merge returns the smallest slot covering both inputs.
func (s Slot) Merge(other Slot) Slot {
	if other.Start < s.Start {
		s.Start = other.Start
	}
	if other.End > s.End {
		s.End = other.End
	}
	return s
}

// Valid reports whether the slot spans a positive, non-overflowing range.
func (s Slot) Valid() bool { return s.End > s.Start }

// String renders the slot as start-end nanoseconds.
func (s Slot) String() string { return fmt.Sprintf("%d-%d", s.Start, s.End) }

// Fault is one observation about the stream, anchored to a timeline span.
type Fault struct {
	Track Track
	Kind  FaultKind
	Seq   uint64 // sequence number the observation is anchored to
	Slot  Slot   // timeline span
	Note  string // optional human detail, kept short and deterministic
}

// String renders the fault for terminal output, one line per fault.
func (f Fault) String() string {
	return fmt.Sprintf("%s %-20s seq=%d start=%d end=%d %s",
		f.Track, f.Kind, f.Seq, f.Slot.Start, f.Slot.End, f.Note)
}

// DamagedRange is a merged span of the timeline that could not be delivered
// as received.  Ranges are produced by DamageSet.Add and are sorted and
// non-overlapping after Merge.
type DamagedRange struct {
	Start int64
	End   int64
	Kind  FaultKind // most severe kind inside the range
	Count int       // number of contributing faults
}

// Slot returns the range as a Slot.
func (r DamagedRange) Slot() Slot { return Slot{Start: r.Start, End: r.End} }

// Duration returns the span length in nanoseconds.
func (r DamagedRange) Duration() int64 { return r.End - r.Start }

// String renders the range in the compact form used by the demo output.
func (r DamagedRange) String() string {
	return fmt.Sprintf("%d-%d:%s", r.Start, r.End, r.Kind)
}

// DamageSet merges overlapping faults into sorted, non-overlapping damaged
// ranges.  It is not safe for concurrent use.
type DamageSet struct {
	ranges []DamagedRange
}

// Add merges f into the set.  Faults with an invalid slot are ignored so a
// corrupt packet cannot poison the range list.
func (d *DamageSet) Add(f Fault) {
	if !f.Slot.Valid() || f.Kind.Severity() == SeverityInfo {
		return
	}
	d.addRange(DamagedRange{Start: f.Slot.Start, End: f.Slot.End, Kind: f.Kind, Count: 1})
}

// AddRange merges an explicit range, used when recover fills a slot.
func (d *DamageSet) AddRange(start, end int64, kind FaultKind) {
	if end <= start || kind.Severity() == SeverityInfo {
		return
	}
	d.addRange(DamagedRange{Start: start, End: end, Kind: kind, Count: 1})
}

func (d *DamageSet) addRange(r DamagedRange) {
	d.ranges = append(d.ranges, r)
	d.normalize()
}

// normalize sorts and coalesces touching or overlapping ranges.  When two
// ranges overlap, the more severe kind wins; equal severities keep the
// earlier range's kind so output is stable across runs.
func (d *DamageSet) normalize() {
	if len(d.ranges) < 2 {
		return
	}
	sortRanges(d.ranges)
	out := d.ranges[:1]
	for _, r := range d.ranges[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End {
			if r.End > last.End {
				last.End = r.End
			}
			last.Count += r.Count
			if r.Kind.Severity() > last.Kind.Severity() {
				last.Kind = r.Kind
			}
			continue
		}
		out = append(out, r)
	}
	d.ranges = out
}

// Ranges returns the merged ranges; callers must not mutate the slice.
func (d *DamageSet) Ranges() []DamagedRange { return d.ranges }

// Len returns the number of merged ranges.
func (d *DamageSet) Len() int { return len(d.ranges) }

// Total returns the summed duration of all ranges in nanoseconds.
func (d *DamageSet) Total() int64 {
	var total int64
	for _, r := range d.ranges {
		total += r.Duration()
	}
	return total
}

// Errors used across the pipeline.  They are sentinel values so callers can
// use errors.Is across package boundaries.
var (
	// ErrCanceled is returned when the caller cancels a run.
	ErrCanceled = errors.New("mediacheck: canceled")
	// ErrLimitExceeded is returned when input violates a configured bound.
	ErrLimitExceeded = errors.New("mediacheck: limit exceeded")
	// ErrUnsupported is returned for features the local build lacks.
	ErrUnsupported = errors.New("mediacheck: unsupported")
	// ErrDamaged is returned when a file was produced but damage was handled.
	ErrDamaged = errors.New("mediacheck: damage handled")
)

func sortRanges(rs []DamagedRange) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Start != rs[j].Start {
			return rs[i].Start < rs[j].Start
		}
		return rs[i].End < rs[j].End
	})
}
