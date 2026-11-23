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
func Build(observed []Observed, opts Options) Map {
	opts = opts.withDefaults()
	m := Map{Entries: make([]Repair, 0, len(observed))}
	m.Digest = digestMap(m.Entries)
	return m
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
