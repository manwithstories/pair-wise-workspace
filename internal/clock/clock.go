// Package clock provides the shared time base used by the repair, probe and
// index stages. Timestamps are normalised into int64 nanoseconds so that
// 33-bit MPEG wraparound, 32-bit RTP values and track timescales can be
// compared without losing precision.
package clock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// WrapModulus is the size of the 33-bit MPEG/transport counter. Callers that
// deal with 32-bit RTP timestamps pass 1<<32 explicitly.
const WrapModulus int64 = 1 << 33

// NanosPerSecond is the common time-base denominator.
const NanosPerSecond = int64(1_000_000_000)

// DefaultTimescale is the 90 kHz clock used by MPEG transport and HLS.
const DefaultTimescale uint32 = 90000

// ToNanos converts a timestamp expressed in track units into nanoseconds. The
// split form avoids overflowing int64 for 33-bit values at 90 kHz.
func ToNanos(value uint64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	ts := int64(timescale)
	v := int64(value)
	return (v/ts)*NanosPerSecond + (v%ts)*NanosPerSecond/ts
}

// FromNanos converts nanoseconds into track units.
func FromNanos(nanos int64, timescale uint32) uint64 {
	if timescale == 0 || nanos <= 0 {
		return 0
	}
	ts := int64(timescale)
	return uint64((nanos/NanosPerSecond)*ts + (nanos%NanosPerSecond)*ts/NanosPerSecond)
}

// NanosDelta converts a signed count of track units into nanoseconds. It is
// the conversion used for values expressed relative to the time-base origin,
// where negative deltas are meaningful (a backward jump).
func NanosDelta(units int64, timescale uint32) int64 {
	if timescale == 0 || units == 0 {
		return 0
	}
	ts := int64(timescale)
	whole := units / ts
	rem := units % ts
	return whole*NanosPerSecond + rem*NanosPerSecond/ts
}

// Millis converts nanoseconds to whole milliseconds (truncating toward zero).
func Millis(nanos int64) int64 {
	return nanos / 1_000_000
}

// Action describes what the deterministic repair did with one segment.
type Action string

const (
	ActionOK          Action = "ok"
	ActionSnapped     Action = "snapped"
	ActionUnwrapped   Action = "unwrapped"
	ActionShifted     Action = "shifted"
	ActionDuplicate   Action = "duplicate"
	ActionGap         Action = "gap"
	ActionUnavailable Action = "unavailable"
)

// Observed is one segment as delivered, in presentation order. Start is the
// raw (possibly wrapping) timestamp in units of the configured timescale.
type Observed struct {
	ID       string
	Start    uint64
	HasStart bool
	Duration int64 // nanoseconds; ignored when <= 0
	Valid    bool  // false means the segment header itself was corrupt
}

// Repair is the deterministic mapping for one observed segment. Start and
// Duration are nanoseconds on the shared base.
type Repair struct {
	ID        string
	Start     int64
	Duration  int64
	Action    Action
	Available bool
	Note      string
}

// Map is the complete repair result for one representation.
type Map struct {
	Entries     []Repair
	Unavailable []string
	Warnings    []string
	Digest      string
}

// Options controls Build.
type Options struct {
	// Modulus is the wrapping counter size; zero selects WrapModulus.
	Modulus int64
	// Timescale is the unit of Observed.Start; zero selects 90000.
	Timescale uint32
	// ToleranceNs is permitted observational jitter before a timestamp is
	// treated as defective. Zero selects 2 ms.
	ToleranceNs int64
	// MaxShiftNs bounds a repairable backward jump. Zero selects four
	// segment durations (per segment, at least 500 ms).
	MaxShiftNs int64
}

func (o Options) withDefaults() Options {
	if o.Modulus <= 0 {
		o.Modulus = WrapModulus
	}
	if o.Timescale == 0 {
		o.Timescale = DefaultTimescale
	}
	if o.ToleranceNs <= 0 {
		o.ToleranceNs = 2_000_000
	}
	return o
}

// Build computes the repair mapping. The algorithm is purely a function of the
// input slice and options, so repeated runs produce byte-identical digests.
// Segments whose timestamp cannot be inferred from an established time base
// are quarantined rather than spliced into the timeline.
//
// The first pass extends every raw timestamp into one monotonic timeline,
// expanding 33-bit counter wraps and rebasing onto a 0-based programme origin.
// The second pass walks the segments against an expected grid and decides,
// purely from the extended values and the neighbouring evidence, whether a
// damaged or missing timestamp is uniquely repairable.
func Build(observed []Observed, opts Options) Map {
	opts = opts.withDefaults()
	n := len(observed)
	warnings := make([]string, 0, 4)
	unavailable := make([]string, 0, 4)

	// Pass 1: extend raw timestamps and find the programme origin.
	ext := make([]int64, n)
	hasTs := make([]bool, n)
	wrapped := make([]bool, n)
	var prevExt int64
	var prevRaw uint64
	seenFirst := false
	for i, obs := range observed {
		if !obs.HasStart {
			continue
		}
		r := int64(obs.Start % uint64(opts.Modulus))
		if !seenFirst {
			ext[i] = r
		} else {
			e := prevExt - int64(prevRaw) + r
			if prevRaw > uint64(r) && prevRaw-uint64(r) > uint64(opts.Modulus/2) {
				e += opts.Modulus
				wrapped[i] = true
			}
			ext[i] = e
		}
		prevExt, prevRaw = ext[i], obs.Start
		seenFirst = true
		hasTs[i] = true
	}
	var origin int64
	for i := range observed {
		if hasTs[i] {
			origin = ext[i]
			break
		}
	}
	rel := func(e int64) int64 { return NanosDelta(e-origin, opts.Timescale) }

	// Pass 2: walk the grid.
	entries := make([]Repair, 0, n)
	var cursor int64
	haveCursor := false
	var lastTsIdx = -2
	for i, obs := range observed {
		dur := obs.Duration
		if dur < 0 {
			dur = 0
		}
		maxShift := opts.MaxShiftNs
		if maxShift <= 0 {
			maxShift = 4 * dur
			if maxShift < 500_000_000 {
				maxShift = 500_000_000
			}
		}

		entry := Repair{ID: obs.ID, Duration: dur, Action: ActionOK, Available: true}
		if wrapped[i] {
			warnings = append(warnings, "wraparound observed and expanded deterministically")
		}

		switch {
		case !obs.Valid:
			// Still record the expected grid slot so a caller can attribute a
			// later sample to this segment by position, then exclude it.
			if haveCursor {
				entry.Start = cursor
			} else if hasTs[i] {
				entry.Start = rel(ext[i])
			}
			entry.Available = false
			entry.Action = ActionUnavailable
			entry.Note = "corrupt segment header"

		case !hasTs[i]:
			if haveCursor && dur > 0 && holeIsUnique(observed, hasTs, i) {
				entry.Start = cursor
				entry.Action = ActionGap
				entry.Note = "missing timestamp inferred from both neighbours"
			} else {
				if haveCursor {
					entry.Start = cursor
				}
				entry.Available = false
				entry.Action = ActionUnavailable
				entry.Note = "missing timestamp cannot be inferred uniquely"
			}

		case !haveCursor:
			entry.Start = rel(ext[i])
			if wrapped[i] {
				entry.Action = ActionUnwrapped
				entry.Note = "wraparound expanded at time-base start"
			}

		default:
			obsNs := rel(ext[i])
			diff := obsNs - cursor
			switch {
			case i-1 == lastTsIdx && obs.Start == observed[i-1].Start:
				entry.Start = cursor
				entry.Action = ActionDuplicate
				entry.Note = "duplicate timestamp re-slotted"
			case abs64(diff) <= opts.ToleranceNs:
				entry.Start = cursor
				if wrapped[i] {
					entry.Action = ActionUnwrapped
					entry.Note = "33-bit wraparound expanded"
				}
			case diff < 0:
				if -diff <= maxShift {
					entry.Start = cursor
					entry.Action = ActionShifted
					entry.Note = fmt.Sprintf("backward jump of %dns repaired", -diff)
				} else {
					entry.Start = cursor
					entry.Available = false
					entry.Action = ActionUnavailable
					entry.Note = fmt.Sprintf("backward jump of %dns exceeds repair limit", -diff)
				}
			default: // forward
				if diff <= 2*opts.ToleranceNs {
					entry.Start = cursor
					entry.Action = ActionSnapped
					entry.Note = fmt.Sprintf("forward jitter of %dns snapped", diff)
				} else {
					entry.Start = obsNs
					entry.Action = ActionGap
					entry.Note = fmt.Sprintf("forward discontinuity of %dns preserved", diff)
					warnings = append(warnings, fmt.Sprintf("%s: forward discontinuity %dns preserved", obs.ID, diff))
				}
			}
		}

		if entry.Available || entry.Action == ActionGap {
			cursor = entry.Start + dur
			haveCursor = true
		} else if haveCursor {
			cursor += dur
		}
		if hasTs[i] {
			lastTsIdx = i
		}
		if !entry.Available {
			unavailable = append(unavailable, entry.ID)
		}
		entries = append(entries, entry)
	}

	m := Map{Entries: entries, Unavailable: unavailable, Warnings: warnings}
	m.Digest = digestMap(entries)
	return m
}

// holeIsUnique reports whether a timestamp-less segment at index i is pinned
// by its neighbours: both are present and valid, and no other hole touches it.
func holeIsUnique(observed []Observed, hasTs []bool, i int) bool {
	if i <= 0 || i+1 >= len(observed) {
		return false
	}
	for _, j := range []int{i - 1, i + 1} {
		if !observed[j].Valid || !hasTs[j] {
			return false
		}
	}
	if i > 1 && !hasTs[i-2] {
		return false
	}
	if i+2 < len(observed) && !hasTs[i+2] {
		return false
	}
	return true
}

// SegmentAt returns the entry whose half-open interval [Start, Start+Duration)
// contains tNs. Entries with a zero duration are skipped. When intervals
// overlap the earliest-starting entry wins, which keeps attribution stable.
func (m Map) SegmentAt(tNs int64) (Repair, bool) {
	var best Repair
	found := false
	for _, e := range m.Entries {
		if e.Duration <= 0 {
			continue
		}
		if tNs >= e.Start && tNs < e.Start+e.Duration {
			if !found || e.Start < best.Start {
				best = e
				found = true
			}
		}
	}
	return best, found
}

// Lookup returns the entry for id.
func (m Map) Lookup(id string) (Repair, bool) {
	for _, e := range m.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return Repair{}, false
}

// DigestEntries returns a stable digest for an entry slice, sorted by ID so
// that callers may pass entries in any order.
func DigestEntries(entries []Repair) string {
	cp := append([]Repair(nil), entries...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	return digestMap(cp)
}

func digestMap(entries []Repair) string {
	cp := append([]Repair(nil), entries...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	var b strings.Builder
	for _, e := range cp {
		state := "0"
		if e.Available {
			state = "1"
		}
		fmt.Fprintf(&b, "%s|%d|%d|%s|%s|%s\n", e.ID, e.Start, e.Duration, e.Action, state, e.Note)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
