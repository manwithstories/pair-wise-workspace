package clock

// MicrosPerSecond is the number of microseconds in one second; sync
// tolerances are expressed on this scale.
const MicrosPerSecond = 1_000_000

// Timeline tracks the expected decode time of a single output track. All
// demux, mapping and remux stages share one Timeline per track so repair
// decisions are consistent across the pipeline.
type Timeline struct {
	Timescale uint32
	next      int64
	valid     bool
}

// NewTimeline returns a Timeline for a track with the given timescale.
func NewTimeline(timescale uint32) *Timeline {
	return &Timeline{Timescale: timescale}
}

// Next reports the expected decode time of the next segment and whether any
// segment has been anchored yet.
func (t *Timeline) Next() (int64, bool) {
	return t.next, t.valid
}

// Advance commits dur timescale units of accepted media.
func (t *Timeline) Advance(dur int64) {
	t.next += dur
	t.valid = true
}

// Skip accounts for dur units of lost or quarantined media so the following
// segment can re-anchor on a continuous timeline.
func (t *Timeline) Skip(dur int64) {
	t.Advance(dur)
}

// Decision is the outcome of resolving a segment's tfdt against the
// timeline.
type Decision struct {
	Repaired     bool   // the tfdt had to be rewritten
	Undetermined bool   // no anchor and no tfdt: quarantine the segment
	Original     uint64 // tfdt as found in the file
	Value        uint64 // decode time to use
	Reason       string // human-readable repair rationale
}

// ResolveTFDT decides the decode time for one segment of this track.
//
// Rules: the first segment anchors the timeline on its own tfdt; a tfdt
// that rolls back behind the expected time is repaired to the expected
// time; a forward leap is adopted (monotonicity is preserved); a missing
// tfdt uses the expected time, and is undetermined when no anchor exists.
func (t *Timeline) ResolveTFDT(hasTFDT bool, value uint64) Decision {
	d := Decision{Original: value, Value: value}
	exp, ok := t.Next()
	switch {
	case !ok && !hasTFDT:
		d.Undetermined = true
		d.Reason = "no tfdt and no timeline anchor"
	case !ok:
		d.Reason = "anchored on first segment tfdt"
	case !hasTFDT:
		d.Value = uint64(exp)
		d.Repaired = true
		d.Reason = "missing tfdt, repaired from timeline"
	case int64(value) < exp:
		d.Value = uint64(exp)
		d.Repaired = true
		d.Reason = "tfdt rollback, repaired from timeline"
	default:
		d.Value = value
		if int64(value) > exp {
			d.Reason = "forward leap adopted"
		}
	}
	return d
}

// Rescale converts v from one timescale to another, rounding to nearest.
func Rescale(v int64, from, to uint32) int64 {
	if from == 0 {
		return 0
	}
	neg := v < 0
	if neg {
		v = -v
	}
	r := (v*int64(to) + int64(from)/2) / int64(from)
	if neg {
		return -r
	}
	return r
}

// ToMicros converts v units of the given timescale to microseconds.
func ToMicros(v int64, timescale uint32) int64 {
	return Rescale(v, timescale, MicrosPerSecond)
}
