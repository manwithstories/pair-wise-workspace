// Package cut partitions validated access units into keyframe-aligned segments.
package cut

import (
	"mediad/internal/timeline"
)

const (
	// DefaultTargetSegmentNS is the desired nominal segment duration.
	DefaultTargetSegmentNS int64 = 1_000_000_000
	// MaxKeyframeWaitNS bounds deferred cuts while waiting for a keyframe.
	MaxKeyframeWaitNS int64 = 2_000_000_000
)

// AU is the timing/view of an access unit used by the segmenter.
type AU struct {
	Index      int
	StartNS    int64
	DurationNS int64
	Keyframe   bool
	Valid      bool
}

// Segment is a finalized keyframe-started time range.
type Segment struct {
	ID             int   `json:"id"`
	StartNS        int64 `json:"start_ns"`
	EndNS          int64 `json:"end_ns"`
	DurationNS     int64 `json:"duration_ns"`
	KeyframeIndex  int   `json:"keyframe_index"`
	AUIndices      []int `json:"au_indices"`
	ContainsBadAUs bool  `json:"contains_bad_aus"`
}

// Config controls segmentation.
type Config struct {
	TargetSegmentNS int64
	MaxNoKeyNS      int64
}

func (c Config) withDefaults() Config {
	if c.TargetSegmentNS <= 0 {
		c.TargetSegmentNS = DefaultTargetSegmentNS
	}
	if c.MaxNoKeyNS <= 0 {
		c.MaxNoKeyNS = MaxKeyframeWaitNS
	}
	return c
}

type openSegment struct {
	seg           Segment
	targetReached bool
	lastAUEndNS   int64
	sawBadAU      bool
}

// Cutter keeps committed segments and one open keyframe-aligned segment.
type Cutter struct {
	cfg       Config
	nextID    int
	open      *openSegment
	Committed []Segment

	Keyframes    int
	ValidAUs     int
	InvalidAUs   int
	DeferredCuts int
}

func New(cfg Config) *Cutter {
	return &Cutter{cfg: cfg.withDefaults()}
}

// Add feeds one AU and returns zero or more newly committed segments.
func (c *Cutter) Add(au AU) ([]Segment, error) {
	if au.Keyframe {
		c.Keyframes++
	}
	if au.Valid {
		c.ValidAUs++
	} else {
		c.InvalidAUs++
	}

	if c.open != nil {
		base := c.open.seg.StartNS
		if au.StartNS-base > c.cfg.MaxNoKeyNS {
			return nil, timeline.New(timeline.KindUnrecoverable, "cut.no_keyframe", au.Index, -1,
				"keyframe gap exceeded %d ns from %d; committed_segments=%d",
				c.cfg.MaxNoKeyNS, base, len(c.Committed))
		}
		if !c.open.targetReached && au.StartNS-base >= c.cfg.TargetSegmentNS {
			c.open.targetReached = true
			c.DeferredCuts++
		}
	}

	var finalized []Segment
	if au.Keyframe && au.Valid {
		if c.open == nil {
			c.start(au)
		} else if c.open.targetReached {
			finalized = append(finalized, c.commit(au.StartNS))
			c.start(au)
		}
		// A keyframe before target duration does not create an extra cut.
	}

	if c.open != nil {
		if !au.Valid {
			c.open.sawBadAU = true
			c.open.seg.ContainsBadAUs = true
		} else {
			c.open.seg.AUIndices = append(c.open.seg.AUIndices, au.Index)
		}
		end := au.StartNS + au.DurationNS
		if end > c.open.lastAUEndNS {
			c.open.lastAUEndNS = end
		}
	}
	return finalized, nil
}

// Flush commits the open segment at its last AU end. A stream with no valid
// keyframe cannot produce a reusable segment.
func (c *Cutter) Flush() ([]Segment, error) {
	if c.open == nil {
		return nil, timeline.New(timeline.KindUnrecoverable, "cut.no_keyframe", -1, -1,
			"no keyframe-aligned segment")
	}
	end := c.open.lastAUEndNS
	if end < c.open.seg.StartNS {
		end = c.open.seg.StartNS
	}
	return []Segment{c.commit(end)}, nil
}

func (c *Cutter) start(au AU) {
	c.open = &openSegment{
		seg: Segment{
			ID:            c.nextID,
			StartNS:       au.StartNS,
			EndNS:         au.StartNS,
			KeyframeIndex: au.Index,
			AUIndices:     nil,
		},
		lastAUEndNS: au.StartNS + au.DurationNS,
	}
	c.nextID++
}

func (c *Cutter) commit(endNS int64) Segment {
	seg := c.open.seg
	seg.EndNS = endNS
	seg.DurationNS = endNS - seg.StartNS
	if seg.DurationNS < 0 {
		seg.DurationNS = 0
	}
	c.Committed = append(c.Committed, seg)
	c.open = nil
	return seg
}

// CommitExternal records a segment that was finalized by Flush or a packer.
// It is mainly useful for resume/recovery bookkeeping.
func (c *Cutter) CommitExternal(seg Segment) {
	c.Committed = append(c.Committed, seg)
	if seg.ID >= c.nextID {
		c.nextID = seg.ID + 1
	}
}
