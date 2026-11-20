// Package clock maps per-camera device timestamps onto the single 90 kHz
// master timeline used by every downstream stage. It handles the four real
// capture defects that make naive arithmetic wrong:
//
//   - different tick rates and tick precision per camera,
//   - 32-bit device timestamp rollover,
//   - duplicate timestamps emitted for consecutive sequence numbers,
//   - sequence-number resets at batch boundaries.
//
// Jumps that cannot be explained by any of those (a clock reset mid-batch,
// a forward jump with no missing frames) are quarantined instead of being
// silently placed on the timeline.
package clock

import (
	"fmt"
	"sort"

	"github.com/mediamend/mediamend/internal/fault"
)

// MasterRate is the unified timeline's ticks per second (RTP video clock).
const MasterRate uint64 = 90000

// WrapMod is the 32-bit device timestamp modulus.
const WrapMod uint64 = 1 << 32

// resetGap is how far the raw sequence must fall for the stream to be treated
// as a new capture batch rather than a duplicate/reordered frame.
const resetGap = 64

// Config declares how one camera stamps its frames.
type Config struct {
	Camera string
	// Rate is the device ticks per second (e.g. 90000, 48000, 1000).
	Rate uint64
	// TickPrecision is the device tick granularity. Two timestamps closer
	// than this are considered "the same instant".
	TickPrecision uint64
	// FrameStep is the device tick distance between consecutive frames.
	FrameStep uint64
	// AnchorSeq/AnchorDeviceTS name the one trusted timestamp in the stream.
	AnchorSeq      uint64
	AnchorDeviceTS uint64
	// AnchorMasterTS is the master-clock position assigned to the anchor.
	AnchorMasterTS uint64
	// MaxDrift is the largest master-tick deviation from the sequence-derived
	// expectation still accepted as jitter (0 = one frame period).
	MaxDrift uint64
	// Quarantine is the deviation beyond which a jump is undecidable and the
	// frame is withheld (0 = half a second).
	Quarantine uint64
}

// Mapped is one frame placed on the master timeline.
type Mapped struct {
	Camera    string
	Seq       uint64
	Logical   uint64 // sequence with batch resets folded out
	MasterTS  uint64
	Inferred  bool // position derived from sequence, not solely the device stamp
	Duplicate bool // another frame already occupied this master timestamp
	Wrapped   bool // a 32-bit device timestamp rollover was expanded
	Note      string
	Offset    int64
}

// QuarantinedFrame is a frame whose timeline position could not be decided.
type QuarantinedFrame struct {
	Seq    uint64
	Offset int64
	Reason string
}

// Result is the outcome of mapping one camera's frames.
type Result struct {
	Camera      string
	Frames      []Mapped
	Quarantined []QuarantinedFrame
	Wraps       int
	Duplicates  int
	Resets      int
	Inferred    int
	Faults      fault.List
}

// Timeline is a stateful mapper for one camera. It is single-goroutine use.
type Timeline struct {
	cfg Config

	havePrev bool
	prevRaw  uint64 // last raw 32-bit device stamp
	prevUnw  uint64 // last expanded (monotonic) device stamp

	haveSeq   bool
	prevSeq   uint64
	seqOffset uint64 // added to raw seq to obtain logical seq

	haveMaster bool
	lastMaster uint64

	// Batch base: the device stamp and master position that the current
	// capture batch's local clock counts from.
	batchBaseRaw    uint64
	batchBaseMaster uint64
	batchBaseSeq    uint64

	seenLogical map[uint64]uint64 // logical seq -> index into Frames
	seenMaster  map[uint64]uint64 // master ts -> logical seq
	framePeriod uint64            // master ticks per frame

	res Result
}

// New validates the configuration and returns a mapper.
func New(cfg Config) (*Timeline, error) {
	if cfg.Camera == "" {
		return nil, fmt.Errorf("clock: empty camera id")
	}
	if cfg.Rate == 0 {
		return nil, fmt.Errorf("clock: camera %s has zero rate", cfg.Camera)
	}
	if cfg.FrameStep == 0 {
		return nil, fmt.Errorf("clock: camera %s has zero frame step", cfg.Camera)
	}
	if cfg.TickPrecision == 0 {
		cfg.TickPrecision = 1
	}
	framePeriod := cfg.toMaster(cfg.FrameStep)
	if framePeriod == 0 {
		framePeriod = 1
	}
	if cfg.MaxDrift == 0 {
		cfg.MaxDrift = framePeriod
	}
	if cfg.Quarantine == 0 {
		cfg.Quarantine = MasterRate / 2
	}
	t := &Timeline{
		cfg:         cfg,
		framePeriod: framePeriod,
		seenLogical: map[uint64]uint64{},
		seenMaster:  map[uint64]uint64{},
		res:         Result{Camera: cfg.Camera},
	}
	t.batchBaseRaw = cfg.AnchorDeviceTS
	t.batchBaseMaster = cfg.AnchorMasterTS
	t.batchBaseSeq = cfg.AnchorSeq
	return t, nil
}

// toMaster converts device ticks to master ticks with nearest rounding.
func (c Config) toMaster(ticks uint64) uint64 {
	if ticks == 0 {
		return 0
	}
	r := c.Rate
	// Guard against overflow for very large stamps: split into seconds and
	// remainder, each of which fits comfortably.
	secs := ticks / r
	rem := ticks % r
	return secs*MasterRate + (rem*MasterRate+r/2)/r
}

// ExpectedMaster returns the master position a raw logical sequence should
// occupy per the anchor, ignoring device timestamps entirely.
func (c Config) ExpectedMaster(logical uint64) uint64 {
	if logical >= c.AnchorSeq {
		return c.AnchorMasterTS + c.toMaster((logical-c.AnchorSeq)*c.FrameStep)
	}
	back := c.toMaster((c.AnchorSeq - logical) * c.FrameStep)
	if back > c.AnchorMasterTS {
		return 0
	}
	return c.AnchorMasterTS - back
}

// Push maps one frame. The second return value is false when the frame was
// quarantined; quarantined frames are recorded in the result but not placed.
func (t *Timeline) Push(seq, deviceTS uint64, offset int64) (Mapped, bool) {
	m := Mapped{Camera: t.cfg.Camera, Seq: seq, Offset: offset}

	// 1. Fold batch-boundary sequence resets into a logical, monotonic seq.
	logical := seq + t.seqOffset
	reset := false
	if t.haveSeq && seq < t.prevSeq && t.prevSeq-seq > resetGap {
		t.seqOffset += t.prevSeq + 1 - seq
		logical = seq + t.seqOffset
		reset = true
	}
	m.Logical = logical

	// 2. Expand a 32-bit device timestamp rollover.
	unwrapped, wrapped := deviceTS, false
	if t.havePrev {
		unwrapped = expand(t.prevUnw, deviceTS)
		wrapped = unwrapped > t.prevUnw && deviceTS < t.prevRaw%WrapMod
	}
	if wrapped {
		t.res.Wraps++
		m.Wrapped = true
	}

	// 3. A batch reset rebases the local device clock at this frame: the new
	// batch counts from zero (or any arbitrary value) on its own clock, so
	// the previous batch's base no longer applies.
	if reset {
		t.res.Resets++
		t.res.Faults = append(t.res.Faults, fault.Fault{
			Kind: fault.KindSequenceWrap, Stage: fault.StageTimeline,
			Camera: t.cfg.Camera, Seq: seq, Offset: offset,
			Message:  fmt.Sprintf("sequence reset %d -> %d treated as a new batch", t.prevSeq, seq),
			Expected: fmt.Sprintf(">= %d", t.prevSeq+1), Actual: fmt.Sprintf("%d", seq),
		})
		t.batchBaseRaw = deviceTS
		t.batchBaseSeq = logical
		if t.haveMaster {
			t.batchBaseMaster = t.lastMaster + t.framePeriod
		} else {
			t.batchBaseMaster = t.cfg.ExpectedMaster(logical)
		}
	}

	// 4. Convert the device stamp into master ticks. Within a batch this is
	// relative to the batch base; the base itself is anchored once.
	var converted uint64
	if unwrapped >= t.batchBaseRaw {
		converted = t.batchBaseMaster + t.cfg.toMaster(unwrapped-t.batchBaseRaw)
	} else if back := t.cfg.toMaster(t.batchBaseRaw - unwrapped); back < t.batchBaseMaster {
		converted = t.batchBaseMaster - back
	}

	// 5. Decide whether the device stamp is trustworthy: compare it with the
	// sequence-derived position. Small drift is jitter; huge drift is either
	// a bad clock (quarantine) or a stale stamp (infer from sequence).
	expected := t.cfg.ExpectedMaster(logical)
	drift := absDiff(converted, expected)
	switch {
	case drift <= int64(t.cfg.MaxDrift):
		m.MasterTS = converted
		m.Note = "device timestamp kept"
	case drift >= int64(t.cfg.Quarantine):
		t.res.Quarantined = append(t.res.Quarantined, QuarantinedFrame{
			Seq: seq, Offset: offset,
			Reason: fmt.Sprintf("undecidable jump: device=%d expected=%d drift=%d ticks", converted, expected, drift),
		})
		t.res.Faults = append(t.res.Faults, fault.Fault{
			Kind: fault.KindTimestampJump, Stage: fault.StageTimeline,
			Camera: t.cfg.Camera, Seq: seq, Offset: offset,
			Message:  "timestamp jump exceeds quarantine threshold",
			Expected: fmt.Sprintf("%d", expected), Actual: fmt.Sprintf("%d", converted),
		})
		return m, false
	default:
		m.MasterTS = expected
		m.Inferred = true
		t.res.Inferred++
		m.Note = fmt.Sprintf("timestamp drift %d ticks exceeds tolerance; inferred from sequence", drift)
	}

	// 6. Duplicate detection: same logical sequence or same master tick.
	if firstLogical, ok := t.seenMaster[m.MasterTS]; ok {
		m.Duplicate = true
		if !m.Inferred {
			m.Inferred = true
			t.res.Inferred++
		}
		t.res.Duplicates++
		t.res.Faults = append(t.res.Faults, fault.Fault{
			Kind: fault.KindDuplicateFrame, Stage: fault.StageTimeline,
			Camera: t.cfg.Camera, Seq: seq, Offset: offset,
			Message:  fmt.Sprintf("master timestamp %d already used by logical seq %d", m.MasterTS, firstLogical),
			Expected: "strictly increasing timestamp", Actual: fmt.Sprintf("%d", m.MasterTS),
		})
	}
	if _, ok := t.seenLogical[logical]; ok {
		m.Duplicate = true
		if !m.Inferred {
			m.Inferred = true
			t.res.Inferred++
		}
	}

	// 7. An unexplained backwards step on the master timeline is recorded but
	// the frame is still placed so downstream stages can reorder it.
	if t.haveMaster && m.MasterTS < t.lastMaster && !m.Duplicate {
		t.res.Faults = append(t.res.Faults, fault.Fault{
			Kind: fault.KindTimestampJump, Stage: fault.StageTimeline,
			Camera: t.cfg.Camera, Seq: seq, Offset: offset,
			Message:  "master timestamp moved backwards",
			Expected: fmt.Sprintf(">= %d", t.lastMaster), Actual: fmt.Sprintf("%d", m.MasterTS),
		})
	}

	if _, ok := t.seenMaster[m.MasterTS]; !ok {
		t.seenMaster[m.MasterTS] = logical
	}
	if _, ok := t.seenLogical[logical]; !ok {
		t.seenLogical[logical] = uint64(len(t.res.Frames))
	}
	t.res.Frames = append(t.res.Frames, m)
	t.lastMaster = m.MasterTS
	t.haveMaster = true
	t.prevRaw = deviceTS
	t.prevUnw = unwrapped
	t.havePrev = true
	t.prevSeq = seq
	t.haveSeq = true
	return m, true
}

// expand resolves a 32-bit rollover of raw relative to the previous unwrapped
// value. A backwards step of at least half the modulus is a rollover; smaller
// backwards steps are returned unchanged (a genuine stale stamp).
func expand(prevUnwrapped, raw uint64) uint64 {
	base := prevUnwrapped - prevUnwrapped%WrapMod
	cand := base + raw
	if cand < prevUnwrapped && prevUnwrapped-cand >= WrapMod/2 {
		cand += WrapMod
	}
	return cand
}

func absDiff(a, b uint64) int64 {
	if a >= b {
		if a-b > 1<<63-1 {
			return 1<<63 - 1
		}
		return int64(a - b)
	}
	if b-a > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(b - a)
}

// Result returns a copy of the accumulated mapping outcome.
func (t *Timeline) Result() Result {
	res := t.res
	res.Frames = append([]Mapped(nil), res.Frames...)
	res.Quarantined = append([]QuarantinedFrame(nil), res.Quarantined...)
	res.Faults = append(fault.List(nil), res.Faults...)
	return res
}

// MapFrames is the convenience entry point for a whole camera stream.
func MapFrames(cfg Config, seqs, deviceTS []uint64, offsets []int64) (Result, error) {
	tl, err := New(cfg)
	if err != nil {
		return Result{}, err
	}
	for i := range seqs {
		off := int64(-1)
		if i < len(offsets) {
			off = offsets[i]
		}
		tl.Push(seqs[i], deviceTS[i], off)
	}
	return tl.Result(), nil
}

// SortedFaults returns the result faults in deterministic order.
func (r Result) SortedFaults() fault.List { return r.Faults.Sorted() }

// Timestamps returns the master timestamps in placement order.
func (r Result) Timestamps() []uint64 {
	out := make([]uint64, len(r.Frames))
	for i := range r.Frames {
		out[i] = r.Frames[i].MasterTS
	}
	return out
}

// CheckStrictlyIncreasing reports the first index where timestamps do not
// strictly increase, or -1 when they do.
func CheckStrictlyIncreasing(ts []uint64) int {
	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			return i
		}
	}
	return -1
}

// SortByMaster orders mapped frames by master timestamp, using sequence as a
// stable tie-breaker.
func SortByMaster(frames []Mapped) {
	sort.SliceStable(frames, func(i, j int) bool {
		if frames[i].MasterTS != frames[j].MasterTS {
			return frames[i].MasterTS < frames[j].MasterTS
		}
		return frames[i].Seq < frames[j].Seq
	})
}
