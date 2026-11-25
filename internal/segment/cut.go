// Package segment cuts the extended-timeline access-unit stream into
// independently publishable fMP4-ready segments at IDR boundaries. A segment
// touched by a continuity-counter gap is explicitly flagged unpublishable.
package segment

import (
	"streamcut/internal/media"
)

// Unit is one timed access unit handed to the cutter. It wraps the shared
// media.AccessUnit with the stream-relative sequence position.
type Unit struct {
	AU       media.AccessUnit
	CCError  bool // this unit's transport packets crossed a CC jump
	ClockGap bool // timestamp could not be proven monotonic
}

// Segment is one cut result. Publishable is false when a CC gap or clock gap
// intersects the segment's time range; such segments stay in the manifest but
// are marked so an edge node must not serve them.
type Segment struct {
	ID          string // deterministic content-derived id (set by fingerprint)
	Index       int
	Start       media.Time90
	End         media.Time90
	Video       []media.AccessUnit
	Audio       []media.AccessUnit
	Publishable bool
	HasCCGap    bool
	HasClockGap bool
	Keyframe    bool // starts at an IDR
}

// Duration reports the segment length in 90 kHz ticks.
func (s *Segment) Duration() media.Time90 { return s.End - s.Start }

// Cutter accumulates units and closes segments at the first IDR at or after
// the target duration. Boundary error is therefore bounded by one GOP.
type Cutter struct {
	target    media.Time90
	cur       *Segment
	index     int
	closed    []Segment
	videoSeen bool
}

// State is the serializable cutter position carried inside a restart
// checkpoint. It captures the open segment so a resumed run continues the
// same cut instead of starting a fresh boundary at the tail's first IDR.
type State struct {
	Target    media.Time90
	Index     int
	VideoSeen bool
	Open      *Segment
}

// Snapshot returns the cutter's resumable state.
func (c *Cutter) Snapshot() State {
	var open *Segment
	if c.cur != nil {
		cp := *c.cur
		open = &cp
	}
	return State{Target: c.target, Index: c.index, VideoSeen: c.videoSeen, Open: open}
}

// Restore rehydrates a cutter from a snapshot taken by a prior run.
func Restore(s State) *Cutter {
	c := &Cutter{target: s.Target, index: s.Index, videoSeen: s.VideoSeen}
	if s.Open != nil {
		open := *s.Open
		c.cur = &open
	}
	return c
}

// NewCutter sets the target segment length.
func NewCutter(target media.Time90) *Cutter {
	return &Cutter{target: target}
}

// Push feeds one unit. A returned *Segment is non-nil exactly when the unit
// closed the previous segment; the unit itself already belongs to the new one.
func (c *Cutter) Push(u Unit) *Segment {
	au := u.AU
	isVideo := au.Kind == media.KindVideoH264
	isIDR := isVideo && au.IsKeyframe()

	if c.cur == nil {
		if isVideo {
			c.cur = c.newSegment(au)
			c.videoSeen = true
			c.applyFlags(u)
		}
		return nil
	}

	var finished *Segment
	if isIDR && au.PTS-c.cur.Start >= c.target {
		finished = c.closeCurrent(au.PTS)
		c.index++ // advance before constructing the next segment's index
		c.cur = c.newSegment(au)
		c.applyFlags(u)
		return c.record(finished)
	}

	if isVideo {
		c.videoSeen = true
		c.cur.Video = append(c.cur.Video, au)
	} else {
		c.cur.Audio = append(c.cur.Audio, au)
	}
	c.applyFlags(u)
	return c.record(finished)
}

// record archives a freshly closed segment and returns it.
func (c *Cutter) record(finished *Segment) *Segment {
	if finished != nil {
		c.closed = append(c.closed, *finished)
	}
	return finished
}

func (c *Cutter) newSegment(first media.AccessUnit) *Segment {
	return &Segment{
		Index:       c.index,
		Start:       first.PTS,
		End:         first.PTS,
		Video:       []media.AccessUnit{first},
		Publishable: true,
		Keyframe:    true,
	}
}

func (c *Cutter) applyFlags(u Unit) {
	if u.CCError {
		c.cur.HasCCGap = true
		c.cur.Publishable = false
	}
	if u.ClockGap || u.AU.ClockGap {
		c.cur.HasClockGap = true
		c.cur.Publishable = false
	}
}

func (c *Cutter) closeCurrent(end media.Time90) *Segment {
	c.cur.End = end
	seg := *c.cur
	return &seg
}

// Flush closes the in-progress segment at end-of-input. Nil when nothing was
// accumulated. The checkpoint machinery only ever treats a flushed segment as
// complete, so cancellation cannot publish a partial unit.
func (c *Cutter) Flush() *Segment {
	if c.cur == nil || (!c.videoSeen && len(c.cur.Video) == 0) {
		return nil
	}
	if len(c.cur.Video) == 0 {
		return nil
	}
	last := c.cur.Video[len(c.cur.Video)-1].PTS
	c.cur.End = last
	seg := *c.cur
	c.cur = nil
	c.closed = append(c.closed, seg)
	c.index++
	return &seg
}

// Segments returns every segment closed so far, including the flushed final
// one in segment order.
func (c *Cutter) Segments() []Segment {
	out := make([]Segment, len(c.closed))
	copy(out, c.closed)
	return out
}
