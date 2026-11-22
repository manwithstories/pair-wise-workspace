// Package segment cuts timed sample streams into seekable segments at
// keyframe boundaries, recording the deviation between target and actual
// cut points.
package segment

import "mediaprobe/internal/mp4"

// TimedSample is a parsed sample placed on the shared media clock.
type TimedSample struct {
	mp4.Sample
	MediaPTS  int64 // shared 90 kHz clock
	MediaDur  int64 // shared 90 kHz clock
	Uncertain bool  // inside a timeline-uncertain interval
	GapBefore bool  // a sequence gap precedes this sample
}

// Segment is a closed run of samples starting at a keyframe.
type Segment struct {
	ID        int
	TrackID   uint32
	Start     int64 // media clock
	End       int64 // media clock
	Samples   []TimedSample
	Uncertain bool
}

// CutRecord logs one cut decision.
type CutRecord struct {
	SegmentID int
	Target    int64 // requested cut point, media clock
	Actual    int64 // keyframe the cut actually landed on
	Deviation int64 // Actual - Target
}

// Cutter closes a segment at the first keyframe at or beyond the target
// duration after the previous cut.
type Cutter struct {
	TargetDuration int64

	segments   []Segment
	cuts       []CutRecord
	cur        []TimedSample
	curStart   int64
	nextTarget int64
	started    bool
	nextID     int
}

// NewCutter creates a cutter with the given target segment duration in
// media-clock units.
func NewCutter(targetDuration int64) *Cutter {
	return &Cutter{TargetDuration: targetDuration}
}

// Add feeds one sample (must be keyframe-flagged video for cutting).
// Samples must arrive in non-decreasing MediaPTS order.
func (c *Cutter) Add(s TimedSample) {
	if !c.started {
		c.curStart, c.nextTarget, c.started = s.MediaPTS, s.MediaPTS+c.TargetDuration, true
	}
	if s.Keyframe && len(c.cur) > 0 && s.MediaPTS >= c.nextTarget {
		c.close(s.MediaPTS)
		c.nextTarget = s.MediaPTS + c.TargetDuration
	}
	c.cur = append(c.cur, s)
}

func (c *Cutter) close(end int64) {
	seg := Segment{
		ID: c.nextID, TrackID: c.cur[0].TrackID,
		Start: c.curStart, End: end, Samples: c.cur,
	}
	for _, s := range c.cur {
		if s.Uncertain {
			seg.Uncertain = true
			break
		}
	}
	c.cuts = append(c.cuts, CutRecord{
		SegmentID: c.nextID, Target: c.nextTarget,
		Actual: end, Deviation: end - c.nextTarget,
	})
	c.nextID++
	c.segments = append(c.segments, seg)
	c.cur = nil
	c.curStart = end
}

// Flush closes the trailing partial segment, if any.
func (c *Cutter) Flush() {
	if len(c.cur) == 0 {
		return
	}
	last := c.cur[len(c.cur)-1]
	c.close(last.MediaPTS + last.MediaDur)
}

// Segments returns all closed segments.
func (c *Cutter) Segments() []Segment { return c.segments }

// Cuts returns the cut decision log.
func (c *Cutter) Cuts() []CutRecord { return c.cuts }
