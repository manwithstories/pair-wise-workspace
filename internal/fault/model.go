// Package fault defines the structured error model shared by manifest
// parsing, fragment probing, indexing, audio normalisation, checkpoints and
// pipelines. Every fault carries the exact segment and byte offset where the
// problem was observed so recovery can resume without guessing.
package fault

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Kind classifies a media or pipeline fault.
type Kind string

const (
	KindManifestParse          Kind = "manifest_parse"
	KindManifestMismatch       Kind = "manifest_mismatch"
	KindPathEscape             Kind = "path_escape"
	KindBoxHeader              Kind = "box_header"
	KindBoxOverflow            Kind = "box_overflow"
	KindMissingMoof            Kind = "missing_moof"
	KindMissingMdat            Kind = "missing_mdat"
	KindTruncatedTail          Kind = "truncated_tail"
	KindTimestampDiscontinuity Kind = "timestamp_discontinuity"
	KindMissingIndex           Kind = "missing_index"
	KindUndecodableAudio       Kind = "undecodable_audio"
	KindCheckpointCorrupt      Kind = "checkpoint_corrupt"
	KindInputChanged           Kind = "input_changed"
	KindHashMismatch           Kind = "hash_mismatch"
	KindLimitExceeded          Kind = "limit_exceeded"
)

var validKinds = map[Kind]bool{
	KindManifestParse: true, KindManifestMismatch: true, KindPathEscape: true,
	KindBoxHeader: true, KindBoxOverflow: true, KindMissingMoof: true,
	KindMissingMdat: true, KindTruncatedTail: true,
	KindTimestampDiscontinuity: true, KindMissingIndex: true,
	KindUndecodableAudio: true, KindCheckpointCorrupt: true,
	KindInputChanged: true, KindHashMismatch: true, KindLimitExceeded: true,
}

// Valid reports whether k is a declared fault kind.
func (k Kind) Valid() bool { return validKinds[k] }

// Location pins a fault to a segment and byte range.
type Location struct {
	Segment string // manifest-relative URI, empty for global faults
	Stream  string // representation or variant identifier
	TrackID uint32
	Offset  int64 // byte offset inside the segment, -1 when not applicable
	Size    int64 // observed or expected size, 0 when unknown
}

// Validate rejects negative offsets other than the sentinel -1.
func (l Location) Validate() error {
	if l.Offset < -1 || l.Size < 0 {
		return fmt.Errorf("fault: invalid location offset=%d size=%d", l.Offset, l.Size)
	}
	return nil
}

// Fault is a single structured observation. Determined states whether the
// affected timestamp/keyframe could be established; undetermined items are
// excluded from hit-rate denominators and never receive fabricated durations.
type Fault struct {
	Kind        Kind
	Loc         Location
	Detail      string
	Recoverable bool
	Determined  bool
}

// New builds and validates a fault.
func New(kind Kind, loc Location, detail string, recoverable, determined bool) (Fault, error) {
	if !kind.Valid() {
		return Fault{}, fmt.Errorf("fault: unknown kind %q", kind)
	}
	if err := loc.Validate(); err != nil {
		return Fault{}, err
	}
	return Fault{Kind: kind, Loc: loc, Detail: detail, Recoverable: recoverable, Determined: determined}, nil
}

// MustNew is New for internal call sites with known-good arguments.
func MustNew(kind Kind, loc Location, detail string, recoverable, determined bool) Fault {
	f, err := New(kind, loc, detail, recoverable, determined)
	if err != nil {
		panic(err)
	}
	return f
}

// Error implements error so a single fault can be returned directly.
func (f Fault) Error() string {
	var b strings.Builder
	b.WriteString(string(f.Kind))
	if f.Loc.Segment != "" {
		b.WriteString(" segment=")
		b.WriteString(f.Loc.Segment)
	}
	if f.Loc.Offset >= 0 {
		fmt.Fprintf(&b, " offset=%d", f.Loc.Offset)
	}
	if f.Detail != "" {
		b.WriteString(": ")
		b.WriteString(f.Detail)
	}
	return b.String()
}

// List is an ordered collection with deterministic serialisation order.
type List struct {
	items []Fault
}

// Add appends faults.
func (l *List) Add(fs ...Fault) { l.items = append(l.items, fs...) }

// Len returns the number of faults.
func (l *List) Len() int { return len(l.items) }

// All returns a sorted copy. Ordering is by segment, offset, kind, detail.
func (l *List) All() []Fault {
	out := make([]Fault, len(l.items))
	copy(out, l.items)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Loc.Segment != b.Loc.Segment {
			return a.Loc.Segment < b.Loc.Segment
		}
		if a.Loc.Offset != b.Loc.Offset {
			return a.Loc.Offset < b.Loc.Offset
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Detail < b.Detail
	})
	return out
}

// ByKind counts faults of each kind.
func (l *List) ByKind() map[Kind]int {
	m := make(map[Kind]int, len(l.items))
	for _, f := range l.items {
		m[f.Kind]++
	}
	return m
}

// FilterKind returns all faults of a kind in deterministic order.
func (l *List) FilterKind(kind Kind) []Fault {
	var out []Fault
	for _, f := range l.All() {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// Recoverable reports whether all recorded faults are recoverable.
func (l *List) Recoverable() bool {
	for _, f := range l.items {
		if !f.Recoverable {
			return false
		}
	}
	return true
}

// Err returns nil when empty, otherwise a joined error.
func (l *List) Err() error {
	if len(l.items) == 0 {
		return nil
	}
	errs := make([]error, 0, len(l.items))
	for _, f := range l.All() {
		errs = append(errs, f)
	}
	return errors.Join(errs...)
}

// SegmentsWithFaults returns sorted unique segment URIs that had faults.
func (l *List) SegmentsWithFaults() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range l.items {
		if f.Loc.Segment != "" && !seen[f.Loc.Segment] {
			seen[f.Loc.Segment] = true
			out = append(out, f.Loc.Segment)
		}
	}
	sort.Strings(out)
	return out
}
