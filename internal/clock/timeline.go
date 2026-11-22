package clock

// MediaTimescale is the shared clock all tracks are aligned to (90 kHz).
const MediaTimescale = 90000

// Rescale converts a timestamp between timescales, rounding to nearest.
func Rescale(ts int64, from, to uint32) int64 {
	if from == 0 || to == 0 || from == to {
		return ts
	}
	v := ts * int64(to)
	f := int64(from)
	if v >= 0 {
		return (v + f/2) / f
	}
	return (v - f/2) / f
}

// JumpKind classifies a discontinuity between consecutive fragments.
type JumpKind int

const (
	JumpNone       JumpKind = iota // continuous
	JumpForward                    // forward gap (missing content)
	JumpRegression                 // tfdt moved backwards
)

func (k JumpKind) String() string {
	switch k {
	case JumpForward:
		return "forward-gap"
	case JumpRegression:
		return "tfdt-regression"
	default:
		return "none"
	}
}

// Interval is a media-clock range flagged with a reason.
type Interval struct {
	Start  int64
	End    int64
	Reason string
}

// TrackTimeline aligns one track's fragments onto the shared media clock.
// Regressions are reported and overlapping ranges are marked uncertain;
// the timeline never silently rewrites timestamps.
type TrackTimeline struct {
	TrackID   uint32
	Timescale uint32

	expected  int64 // expected next tfdt, track units
	hasExpect bool

	Regressions int
	ForwardGaps int
	Uncertain   []Interval
}

// NewTrackTimeline creates a timeline for one track.
func NewTrackTimeline(trackID, timescale uint32) *TrackTimeline {
	return &TrackTimeline{TrackID: trackID, Timescale: timescale}
}

// Align registers a fragment by its tfdt and total sample duration (both in
// track units) and returns the fragment's media-clock start plus the jump
// classification relative to the previous fragment.
func (t *TrackTimeline) Align(tfdt, durSum int64) (mediaStart int64, kind JumpKind) {
	mediaStart = Rescale(tfdt, t.Timescale, MediaTimescale)
	kind = JumpNone
	if t.hasExpect {
		switch {
		case tfdt < t.expected:
			t.Regressions++
			kind = JumpRegression
			t.Uncertain = append(t.Uncertain, Interval{
				Start:  mediaStart,
				End:    Rescale(t.expected, t.Timescale, MediaTimescale),
				Reason: "tfdt regression overlaps prior timeline",
			})
		case tfdt > t.expected:
			t.ForwardGaps++
			kind = JumpForward
		}
	}
	if !t.hasExpect || tfdt+durSum > t.expected {
		t.expected = tfdt + durSum
	}
	t.hasExpect = true
	return mediaStart, kind
}

// Timeline manages the per-track timelines sharing one media clock.
type Timeline struct {
	tracks map[uint32]*TrackTimeline
}

// NewTimeline returns an empty multi-track timeline.
func NewTimeline() *Timeline { return &Timeline{tracks: map[uint32]*TrackTimeline{}} }

// Track returns (creating if needed) the timeline for trackID.
func (tl *Timeline) Track(trackID, timescale uint32) *TrackTimeline {
	t, ok := tl.tracks[trackID]
	if !ok {
		t = NewTrackTimeline(trackID, timescale)
		tl.tracks[trackID] = t
	}
	return t
}

// Tracks returns all known track timelines.
func (tl *Timeline) Tracks() map[uint32]*TrackTimeline { return tl.tracks }
